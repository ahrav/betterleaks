package validate

import (
	"sync"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/google/cel-go/cel"
)

// ValidationResult pairs a finding's fingerprint with its validation outcome.
type ValidationResult struct {
	Fingerprint string
	Status      string
	Reason      string
	Meta        map[string]any
}

// validationJob is the internal unit of work for the pool.
type validationJob struct {
	fingerprint string
	ruleID      string
	program     any // cel.Program
	secret      string
	captures    map[string]string
	required    map[string]string
}

// statusPriority defines the preference order when merging results for the same
// fingerprint. Lower number = higher priority ("valid" wins over everything).
var statusPriority = map[string]int{
	"valid":   0,
	"invalid": 1,
	"revoked": 2,
	"unknown": 3,
	"error":   4,
}

// fingerprintState tracks combo results for a single fingerprint.
type fingerprintState struct {
	best     ValidationResult
	hasBest  bool
	pending  int
	resolved bool
}

// Pool manages a set of workers that validate findings asynchronously.
type Pool struct {
	env   *Environment
	cache *Cache
	jobs  chan validationJob
	wg    sync.WaitGroup

	mu     sync.Mutex
	states map[string]*fingerprintState
	order  []string // insertion order for Results()

	// OnResult is called when a fingerprint is fully resolved (all combo jobs
	// done, or a "valid" short-circuits remaining combos). The callback is
	// invoked from a worker goroutine; it must be safe for concurrent use.
	OnResult func(ValidationResult)
}

// NewPool creates a validation pool with the given number of workers.
func NewPool(workers int, env *Environment) *Pool {
	if workers <= 0 {
		workers = 10
	}
	p := &Pool{
		env:    env,
		cache:  NewCache(),
		jobs:   make(chan validationJob, workers*10),
		states: make(map[string]*fingerprintState),
	}

	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}

	return p
}

// Submit queues a finding for validation. count is the total number of jobs
// that will be submitted for this fingerprint (1 for simple findings, N for
// combo expansion). All Submit calls for the same fingerprint must use the
// same count value.
func (p *Pool) Submit(fingerprint string, ruleID string, program any, secret string, captures map[string]string, required map[string]string, count int) {
	p.mu.Lock()
	if _, ok := p.states[fingerprint]; !ok {
		p.states[fingerprint] = &fingerprintState{pending: count}
		p.order = append(p.order, fingerprint)
	}
	p.mu.Unlock()

	p.jobs <- validationJob{
		fingerprint: fingerprint,
		ruleID:      ruleID,
		program:     program,
		secret:      secret,
		captures:    captures,
		required:    required,
	}
}

// Close signals that no more jobs will be submitted and waits for all workers to finish.
func (p *Pool) Close() {
	close(p.jobs)
	p.wg.Wait()
}

// Stats returns cache hit/miss counts. Must be called after Close().
func (p *Pool) Stats() (hits, misses uint64) {
	return p.cache.Hits(), p.cache.Misses()
}

// Results returns the final validation results in submission order.
// Must be called after Close().
func (p *Pool) Results() []ValidationResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	results := make([]ValidationResult, 0, len(p.order))
	for _, fp := range p.order {
		if state, ok := p.states[fp]; ok && state.hasBest {
			results = append(results, state.best)
		}
	}
	return results
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		vr := ValidationResult{
			Fingerprint: job.fingerprint,
			Meta:        map[string]any{},
		}

		prg, ok := job.program.(cel.Program)
		if !ok {
			logging.Warn().
				Str("rule", job.ruleID).
				Msg("validation job has invalid program type")
			vr.Status = "error"
			vr.Reason = "invalid CEL program type"
		} else {
			cacheKey := CacheKey(job.ruleID, job.secret, job.required)
			// Merge required rule secrets into captures so the CEL
			// program can reference them (e.g. captures["rule-id"]).
			merged := job.captures
			if len(job.required) > 0 {
				merged = make(map[string]string, len(job.captures)+len(job.required))
				for k, v := range job.captures {
					merged[k] = v
				}
				for k, v := range job.required {
					merged[k] = v
				}
			}
			result, err := p.cache.GetOrDo(cacheKey, func() (*Result, error) {
				return p.env.Eval(prg, job.secret, merged)
			})
			if err != nil {
				vr.Status = "error"
				vr.Reason = err.Error()
			} else {
				vr.Status = result.Status
				vr.Reason = result.Reason
				vr.Meta = result.Metadata
			}
		}

		p.mu.Lock()
		state := p.states[job.fingerprint]
		state.pending--

		// Update best if this result has higher priority.
		newPri := statusPriority[vr.Status]
		if !state.hasBest || newPri < statusPriority[state.best.Status] {
			state.best = vr
			state.hasBest = true
		}

		// Resolve when all combos done OR "valid" short-circuits.
		shouldFire := false
		if !state.resolved && (state.pending <= 0 || vr.Status == "valid") {
			state.resolved = true
			shouldFire = true
		}
		best := state.best
		p.mu.Unlock()

		if shouldFire && p.OnResult != nil {
			p.OnResult(best)
		}
	}
}
