package detect

import "sync"

// occRecorder captures keyword occurrence positions during the prefilter
// scan so hit-window construction (rule_window.go) can reuse them instead
// of re-discovering occurrences with bytes.Index over the whole fragment.
//
// Soundness contract: windows must cover EVERY occurrence of a rule's
// keywords, so a pattern's recorded list is usable only if it is complete.
// Each pattern's list is capped; on overflow the pattern is flagged and
// its rules fall back to the full-fragment scan (always sound). The cap
// also bounds the extra verification cost the prefilter pays for repeated
// occurrences of high-frequency keywords: once overflowed, the scan
// reverts to its seen-shortcut behavior for that pattern.
type occRecorder struct {
	// track[pattern] enables recording for patterns that map to at least
	// one window-eligible rule (set once at detector construction; shared
	// read-only slice, not owned by the recorder).
	track []bool
	// pos[pattern] holds occurrence start offsets, complete unless
	// overflow[pattern].
	pos      [][]int32
	overflow []bool
	// touched lists patterns with recorded state, for O(touched) reset.
	touched []uint32
}

// occRecorderCap bounds positions kept per pattern. A pattern occurring
// more often than this in one fragment would produce windows covering
// most of the fragment anyway (the coverage fallback in ruleWindows), so
// completeness past the cap has almost no value.
const occRecorderCap = 48

func newOccRecorder(patterns int, track []bool) *occRecorder {
	return &occRecorder{
		track:    track,
		pos:      make([][]int32, patterns),
		overflow: make([]bool, patterns),
	}
}

// record notes an occurrence of pattern starting at start. Returns false
// when the caller may stop reporting this pattern's occurrences (not
// tracked, or overflowed) — the prefilter uses this to fall back to its
// dedupe shortcut.
func (r *occRecorder) record(pattern uint32, start int32) bool {
	if !r.track[pattern] || r.overflow[pattern] {
		return false
	}
	lst := r.pos[pattern]
	if len(lst) == 0 {
		r.touched = append(r.touched, pattern)
	}
	if len(lst) >= occRecorderCap {
		r.overflow[pattern] = true
		return false
	}
	r.pos[pattern] = append(lst, start)
	return true
}

// wants reports whether occurrences of pattern still need reporting.
func (r *occRecorder) wants(pattern uint32) bool {
	return r.track[pattern] && !r.overflow[pattern]
}

// positions returns the complete occurrence list for pattern, or
// (nil, false) when the list is unusable (overflowed).
func (r *occRecorder) positions(pattern uint32) ([]int32, bool) {
	if r.overflow[pattern] {
		return nil, false
	}
	return r.pos[pattern], true
}

// reset clears only touched entries, so pooled reuse is O(touched).
func (r *occRecorder) reset() {
	for _, p := range r.touched {
		r.pos[p] = r.pos[p][:0]
		r.overflow[p] = false
	}
	r.touched = r.touched[:0]
}

var occRecorderPool sync.Pool

// getOccRecorder returns a pooled recorder sized for patterns; track is
// the detector's static per-pattern eligibility slice.
func getOccRecorder(patterns int, track []bool) *occRecorder {
	if v := occRecorderPool.Get(); v != nil {
		r := v.(*occRecorder)
		if len(r.pos) == patterns {
			r.track = track
			return r
		}
	}
	return newOccRecorder(patterns, track)
}

func putOccRecorder(r *occRecorder) {
	r.reset()
	occRecorderPool.Put(r)
}
