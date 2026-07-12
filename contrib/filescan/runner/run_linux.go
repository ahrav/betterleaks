//go:build linux

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/corpus"
	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

const (
	maxScannerMetricsBytes      = 16 << 20
	confirmationOracleCandidate = "baseline/parallel"
)

type loadedStratum struct {
	plan           stratum
	manifest       *corpus.Manifest
	manifestSHA256 string
}

func executePlan(ctx context.Context, plan plan, planDigest, resultsPath string) (runSummary, error) {
	binaryDigest, err := corpus.FileSHA256(plan.Binary)
	if err != nil {
		return runSummary{}, fmt.Errorf("hash benchmark binary: %w", err)
	}
	if plan.BinarySHA256 != "" && !strings.EqualFold(plan.BinarySHA256, binaryDigest) {
		return runSummary{}, fmt.Errorf("binary sha256 %s, want %s", binaryDigest, plan.BinarySHA256)
	}
	workingInfo, err := os.Stat(plan.WorkingDirectory)
	if err != nil {
		return runSummary{}, fmt.Errorf("stat working directory: %w", err)
	}
	if !workingInfo.IsDir() {
		return runSummary{}, errors.New("working_directory is not a directory")
	}
	for _, artifact := range plan.Artifacts {
		actual, err := corpus.FileSHA256(artifact.Path)
		if err != nil {
			return runSummary{}, fmt.Errorf("hash artifact %q: %w", artifact.Path, err)
		}
		if !strings.EqualFold(actual, artifact.SHA256) {
			return runSummary{}, fmt.Errorf("artifact %q sha256 %s, want %s", artifact.Path, actual, artifact.SHA256)
		}
	}
	loadedStrata, err := loadStrata(plan)
	if err != nil {
		return runSummary{}, err
	}
	resultsPath, err = filepath.Abs(resultsPath)
	if err != nil {
		return runSummary{}, fmt.Errorf("resolve results path: %w", err)
	}
	for _, stratum := range loadedStrata {
		inside, err := isWithin(stratum.plan.Root, resultsPath)
		if err != nil {
			return runSummary{}, err
		}
		if inside {
			return runSummary{}, fmt.Errorf("results path must be outside corpus root %q", stratum.plan.Root)
		}
	}
	writer, err := newJSONLWriter(resultsPath)
	if err != nil {
		return runSummary{}, err
	}
	closed := false
	defer func() {
		if !closed {
			_, _, _ = writer.Close()
		}
	}()

	candidates := make(map[string]candidate, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		candidates[candidate.ID] = candidate
	}
	replacements := 0
	for _, loaded := range loadedStrata {
		schedule, err := buildSchedule(plan.Candidates, plan.FixedHorizonBlocks, plan.Seed, loaded.plan.ID, plan.Phase)
		if err != nil {
			return runSummary{}, err
		}
		for _, block := range schedule {
			attempt := 0
			for {
				if err := ctx.Err(); err != nil {
					return runSummary{}, err
				}
				rows, infrastructureInvalid := runBlock(ctx, plan, planDigest, binaryDigest, loaded, block, attempt, candidates)
				if infrastructureInvalid {
					for i := range rows {
						rows[i].BlockValidity = validityInfrastructure
					}
				} else {
					for i := range rows {
						rows[i].BlockValidity = validityValid
					}
				}
				if err := writer.WriteBlock(rows); err != nil {
					return runSummary{}, err
				}
				if !infrastructureInvalid {
					break
				}
				replacements++
				attempt++
				if replacements > plan.MaxInfrastructureReplacements {
					return runSummary{}, fmt.Errorf("infrastructure replacement cap exceeded after block %d in stratum %s", block.Index, loaded.plan.ID)
				}
			}
		}
	}
	resultsDigest, rows, err := writer.Close()
	closed = true
	if err != nil {
		return runSummary{}, err
	}
	return runSummary{
		SchemaVersion: resultSchemaVersion, StudyID: plan.StudyID,
		PlanSHA256: planDigest, ResultsPath: resultsPath, ResultsSHA256: resultsDigest,
		Rows: rows, InfrastructureReplacementBlocks: replacements,
	}, nil
}

func loadStrata(plan plan) ([]loadedStratum, error) {
	loaded := make([]loadedStratum, 0, len(plan.Strata))
	for _, stratum := range plan.Strata {
		manifestDigest, err := corpus.FileSHA256(stratum.CorpusManifest)
		if err != nil {
			return nil, fmt.Errorf("hash stratum %q manifest: %w", stratum.ID, err)
		}
		if stratum.ManifestSHA256 != "" && manifestDigest != stratum.ManifestSHA256 {
			return nil, fmt.Errorf(
				"stratum %q manifest sha256 %s, want %s",
				stratum.ID, manifestDigest, stratum.ManifestSHA256,
			)
		}
		manifest, err := corpus.Load(stratum.CorpusManifest)
		if err != nil {
			return nil, fmt.Errorf("load stratum %q manifest: %w", stratum.ID, err)
		}
		if manifest.WorkloadClass != stratum.WorkloadClass {
			return nil, fmt.Errorf("stratum %q workload_class %q differs from manifest %q", stratum.ID, stratum.WorkloadClass, manifest.WorkloadClass)
		}
		expectedRole := "calibration"
		if stratum.EvidenceRole == "correctness" {
			expectedRole = "correctness"
		} else if plan.Phase == "confirmation" {
			expectedRole = "confirmation"
		}
		if manifest.Role != expectedRole {
			return nil, fmt.Errorf(
				"stratum %q evidence_role %q in phase %q requires manifest role %q, got %q",
				stratum.ID, stratum.EvidenceRole, plan.Phase, expectedRole, manifest.Role,
			)
		}
		if plan.Phase == "confirmation" &&
			(manifest.Oracle == nil || manifest.Oracle.Candidate != confirmationOracleCandidate) {
			candidate := ""
			if manifest.Oracle != nil {
				candidate = manifest.Oracle.Candidate
			}
			return nil, fmt.Errorf(
				"confirmation stratum %q oracle candidate %q, want %q",
				stratum.ID, candidate, confirmationOracleCandidate,
			)
		}
		if !stratum.SkipLiveGuard && filepath.Clean(manifest.Root) != filepath.Clean(stratum.Root) {
			return nil, fmt.Errorf("stratum %q root differs from live-guard manifest root; recreate the manifest or set skip_live_guard", stratum.ID)
		}
		if manifest.Summary.ManifestErrors != 0 && !stratum.AllowManifestErrors {
			return nil, fmt.Errorf("stratum %q manifest contains %d errors", stratum.ID, manifest.Summary.ManifestErrors)
		}
		if plan.RequireScannerMetrics {
			for name, identity := range map[string]protocol.OutputIdentity{
				"targets":   manifest.ExpectedOutputs.Targets,
				"fragments": manifest.ExpectedOutputs.Fragments,
				"findings":  manifest.ExpectedOutputs.Findings,
				"errors":    manifest.ExpectedOutputs.Errors,
				"fallbacks": manifest.ExpectedOutputs.Fallbacks,
			} {
				if identity.Digest == "" {
					return nil, fmt.Errorf("stratum %q has no frozen %s oracle", stratum.ID, name)
				}
			}
		}
		loadedDigest, err := corpus.FileSHA256(stratum.CorpusManifest)
		if err != nil {
			return nil, fmt.Errorf("rehash stratum %q manifest: %w", stratum.ID, err)
		}
		if loadedDigest != manifestDigest {
			return nil, fmt.Errorf("stratum %q manifest changed while it was loaded", stratum.ID)
		}
		loaded = append(loaded, loadedStratum{plan: stratum, manifest: manifest, manifestSHA256: manifestDigest})
	}
	return loaded, nil
}

func runBlock(ctx context.Context, plan plan, planDigest, binaryDigest string, loaded loadedStratum, block scheduledBlock, attempt int, candidates map[string]candidate) ([]runResult, bool) {
	rows := make([]runResult, 0, len(block.Candidates))
	for position, candidateID := range block.Candidates {
		if ctx.Err() != nil {
			return rows, true
		}
		candidate := candidates[candidateID]
		row := newRunResult(plan, planDigest, binaryDigest, loaded, block, attempt, position, candidate)
		if !loaded.plan.SkipLiveGuard {
			verification, err := corpus.Verify(ctx, loaded.plan.CorpusManifest, loaded.plan.Root, "live")
			if err != nil {
				row.ValidityLedger = append(row.ValidityLedger, protocol.LedgerEntry{
					Code: "live_guard_before_error", Severity: "error", Owner: "infrastructure", Message: err.Error(),
				})
			} else {
				row.LiveGuardBefore = &verification
				row.ValidityLedger = append(row.ValidityLedger, verification.Validity...)
			}
			if classifyValidity(row.ValidityLedger) == validityInfrastructure {
				row.Validity = validityInfrastructure
				rows = append(rows, row)
				return rows, true
			}
		}
		// Verify immutability before conditioning cache state. A controlled-cold
		// hook must be the final filesystem operation before the timed scanner;
		// the after guard still detects any mutation made by a hook or candidate.
		for _, hook := range append(append([]hook(nil), plan.PreRunHooks...), loaded.plan.PrepareHooks...) {
			result := runHook(ctx, plan, hook)
			row.Hooks = append(row.Hooks, result)
			if result.TimedOut || result.ExitCode != 0 {
				row.ValidityLedger = append(row.ValidityLedger, protocol.LedgerEntry{
					Code: "pre_run_hook_failed", Severity: "error", Owner: "infrastructure",
					Message: fmt.Sprintf("hook %s exit=%d timed_out=%t", hook.ID, result.ExitCode, result.TimedOut),
				})
				row.Validity = classifyValidity(row.ValidityLedger)
				rows = append(rows, row)
				return rows, true
			}
		}

		runScanner(ctx, plan, candidate, loaded, &row)
		if err := ctx.Err(); err != nil {
			row.ValidityLedger = append(row.ValidityLedger, protocol.LedgerEntry{
				Code: "harness_context_canceled", Severity: "error", Owner: "infrastructure", Message: err.Error(),
			})
			row.Validity = validityInfrastructure
			rows = append(rows, row)
			return rows, true
		}
		if !loaded.plan.SkipLiveGuard {
			verification, err := corpus.Verify(ctx, loaded.plan.CorpusManifest, loaded.plan.Root, "live")
			if err != nil {
				row.ValidityLedger = append(row.ValidityLedger, protocol.LedgerEntry{
					Code: "live_guard_after_error", Severity: "error", Owner: "infrastructure", Message: err.Error(),
				})
			} else {
				row.LiveGuardAfter = &verification
				row.ValidityLedger = append(row.ValidityLedger, verification.Validity...)
			}
		}
		row.Validity = classifyValidity(row.ValidityLedger)
		rows = append(rows, row)
		if row.Validity == validityInfrastructure {
			return rows, true
		}
	}
	return rows, false
}

func newRunResult(plan plan, planDigest, binaryDigest string, loaded loadedStratum, block scheduledBlock, attempt, position int, candidate candidate) runResult {
	args := make([]string, 0, 1+len(plan.CommonArgs)+len(candidate.Args)+len(loaded.plan.ScannerArgs))
	args = append(args, plan.Binary)
	args = append(args, plan.CommonArgs...)
	// ScannerArgs starts with the Betterleaks subcommand. Candidate arguments
	// are subcommand-local experiment flags and must follow it.
	args = append(args, loaded.plan.ScannerArgs[0])
	args = append(args, candidate.Args...)
	args = append(args, loaded.plan.ScannerArgs[1:]...)
	_, overrides, environmentDigest := commandEnvironment(plan.InheritEnvironment, plan.Environment, candidate.Environment, map[string]string{metricsFDEnvironment: "3"})
	return runResult{
		SchemaVersion: resultSchemaVersion, StudyID: plan.StudyID, Phase: plan.Phase,
		PlanSHA256: planDigest, BinarySHA256: binaryDigest,
		Artifacts: append([]artifact{}, plan.Artifacts...), ManifestSHA256: loaded.manifestSHA256,
		StratumID: loaded.plan.ID, WorkloadClass: loaded.plan.WorkloadClass,
		EvidenceRole: loaded.plan.EvidenceRole, CacheState: loaded.plan.CacheState,
		AllowedExitCodes: loaded.plan.effectiveAllowedExitCodes(),
		BlockIndex:       block.Index, BlockAttempt: attempt, SequenceID: block.SequenceID,
		SequencePosition: position, CandidateID: candidate.ID,
		MetricsCandidate: candidate.MetricsCandidate, Command: args,
		WorkingDirectory: plan.WorkingDirectory, Environment: overrides, EnvironmentSHA256: environmentDigest,
		Exit:     exitResult{Code: -1},
		Host:     newHostTelemetrySkeleton(loaded.plan.Root, time.Duration(plan.SampleIntervalMS)*time.Millisecond),
		Validity: validityValid,
	}
}

func runScanner(ctx context.Context, plan plan, candidate candidate, loaded loadedStratum, result *runResult) {
	command := result.Command
	environment, _, _ := commandEnvironment(plan.InheritEnvironment, plan.Environment, candidate.Environment, map[string]string{metricsFDEnvironment: "3"})
	metricsReader, metricsWriter, err := os.Pipe()
	if err != nil {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "metrics_pipe_error", Severity: "error", Owner: "infrastructure", Message: err.Error(),
		})
		return
	}
	defer metricsReader.Close()

	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(plan.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, command[0], command[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = plan.WorkingDirectory
	cmd.Env = environment
	cmd.ExtraFiles = []*os.File{metricsWriter}
	stdout := newBoundedBuffer(maxCapturedOutputBytes)
	stderr := newBoundedBuffer(maxCapturedOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	hostSampler := newHostSampler(loaded.plan.Root, time.Duration(plan.SampleIntervalMS)*time.Millisecond)
	hostSampler.sample()

	metricsChannel := make(chan metricsRead, 1)
	go func() {
		data, readErr := io.ReadAll(io.LimitReader(metricsReader, maxScannerMetricsBytes+1))
		metricsChannel <- metricsRead{data: data, err: readErr}
	}()

	start := time.Now()
	result.StartUTC = start.UTC()
	if err := cmd.Start(); err != nil {
		_ = metricsWriter.Close()
		_ = metricsReader.Close()
		<-metricsChannel
		result.EndUTC = time.Now().UTC()
		result.WallNS = time.Since(start).Nanoseconds()
		result.Exit.WaitError = err.Error()
		hostSampler.sample()
		result.Host = hostSampler.finish()
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "process_start_failed", Severity: "error", Owner: "infrastructure", Message: err.Error(),
		})
		return
	}
	result.Started = true
	_ = metricsWriter.Close()
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	hostCtx, stopHost := context.WithCancel(context.Background())
	resourcesChannel := make(chan procResourceSummary, 1)
	hostChannel := make(chan hostTelemetry, 1)
	go func() {
		resourcesChannel <- sampleProcessTree(samplerCtx, cmd.Process.Pid, time.Duration(plan.SampleIntervalMS)*time.Millisecond)
	}()
	go func() {
		hostChannel <- hostSampler.run(hostCtx, time.Duration(plan.SampleIntervalMS)*time.Millisecond)
	}()

	waitErr := cmd.Wait()
	result.EndUTC = time.Now().UTC()
	result.WallNS = time.Since(start).Nanoseconds()
	stopSampler()
	stopHost()
	result.Procfs = <-resourcesChannel
	result.Host = <-hostChannel
	result.Exit = processExit(cmd, commandCtx, waitErr)
	result.Rusage = processRusage(cmd)
	recordProcfsValidity(result)
	result.Stdout, result.StdoutTruncated = stdout.snapshot()
	result.Stderr, result.StderrTruncated = stderr.snapshot()
	if result.StdoutTruncated {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "stdout_capture_truncated", Severity: "error", Owner: "candidate",
		})
	}
	if result.StderrTruncated {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "stderr_capture_truncated", Severity: "error", Owner: "candidate",
		})
	}
	recordExitValidity(waitErr, result)

	var metricsRead metricsRead
	select {
	case metricsRead = <-metricsChannel:
	case <-time.After(2 * time.Second):
		// A descendant retained the candidate-owned metrics descriptor after
		// the root process exited. Kill the isolated process group before
		// forcing the read closed so the invalid run leaves no stray worker.
		_ = cmd.Cancel()
		_ = metricsReader.Close()
		metricsRead = <-metricsChannel
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "metrics_fd_close_timeout", Severity: "error", Owner: "candidate",
		})
	}
	consumeScannerMetrics(plan, loaded.manifest, metricsRead, result)
}

func recordProcfsValidity(result *runResult) {
	if result.Procfs.Samples != 0 {
		return
	}
	if result.Procfs.SampleErrors != 0 {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "procfs_sampling_failed", Severity: "error", Owner: "infrastructure",
			Message: fmt.Sprintf("%d procfs sampling attempts failed without a usable sample", result.Procfs.SampleErrors),
		})
		return
	}
	// A sub-interval process can be reaped before the sampler observes /proc.
	// Wait4 still supplies its root-process usage, so this is not a failed run.
	result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
		Code: "procfs_no_samples", Severity: "warning", Owner: "harness",
		Message: "process was not observed before wait4 completed; root-process wait4 usage remains available",
	})
}

func recordExitValidity(waitErr error, result *runResult) {
	message := ""
	if waitErr != nil {
		message = waitErr.Error()
	}
	if result.Exit.TimedOut {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "process_timeout", Severity: "error", Owner: "candidate", Message: message,
		})
		return
	}
	if result.Exit.Signal != "" {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "process_signaled", Severity: "error", Owner: "candidate", Message: message,
		})
		return
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
				Code: "process_wait_error", Severity: "error", Owner: "infrastructure", Message: message,
			})
			return
		}
	}
	for _, allowed := range result.AllowedExitCodes {
		if result.Exit.Code != allowed {
			continue
		}
		if result.Exit.Code != 0 {
			result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
				Code: "allowed_nonzero_exit", Severity: "info", Owner: "candidate",
				Message: fmt.Sprintf("exit code %d was predeclared for this correctness stratum", result.Exit.Code),
			})
		}
		return
	}
	result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
		Code: "unexpected_exit_code", Severity: "error", Owner: "candidate",
		Message: fmt.Sprintf("exit code %d; allowed %v", result.Exit.Code, result.AllowedExitCodes),
	})
}

type metricsRead struct {
	data []byte
	err  error
}

func consumeScannerMetrics(plan plan, manifest *corpus.Manifest, read metricsRead, result *runResult) {
	if read.err != nil && !errors.Is(read.err, os.ErrClosed) {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "metrics_read_error", Severity: "error", Owner: "infrastructure", Message: read.err.Error(),
		})
	}
	if len(read.data) > maxScannerMetricsBytes {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "metrics_too_large", Severity: "error", Owner: "candidate",
		})
		return
	}
	if len(strings.TrimSpace(string(read.data))) == 0 {
		severity := "warning"
		owner := "harness"
		if plan.RequireScannerMetrics {
			severity = "error"
			owner = "candidate"
		}
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "scanner_metrics_missing", Severity: severity, Owner: owner,
		})
		return
	}
	result.ScannerMetricsSHA256 = sha256Hex(read.data)
	metrics, err := protocol.DecodeScannerMetrics(read.data)
	if err != nil {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "scanner_metrics_invalid", Severity: "error", Owner: "candidate", Message: err.Error(),
		})
		return
	}
	result.ScannerMetrics = &metrics
	if result.MetricsCandidate != "" && metrics.Candidate != result.MetricsCandidate {
		result.ValidityLedger = append(result.ValidityLedger, protocol.LedgerEntry{
			Code: "scanner_candidate_mismatch", Severity: "error", Owner: "candidate",
			Message: fmt.Sprintf("metrics candidate %q; expected %q", metrics.Candidate, result.MetricsCandidate),
		})
	}
	result.ValidityLedger = append(result.ValidityLedger, metrics.Validity...)
	result.ValidityLedger = append(result.ValidityLedger, protocol.CompareExpected(manifest.ExpectedOutputs, metrics.Outputs())...)
}

func processExit(cmd *exec.Cmd, ctx context.Context, waitErr error) exitResult {
	result := exitResult{Code: -1, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if cmd.ProcessState != nil {
		result.Code = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = status.Signal().String()
		}
	}
	if waitErr != nil {
		result.WaitError = waitErr.Error()
	}
	return result
}

func processRusage(cmd *exec.Cmd) waitResourceUsage {
	result := waitResourceUsage{Scope: "wait4_root_process_including_kernel_accounted_waited_children"}
	if cmd.ProcessState == nil {
		return result
	}
	result.UserCPU_NS = cmd.ProcessState.UserTime().Nanoseconds()
	result.SystemCPU_NS = cmd.ProcessState.SystemTime().Nanoseconds()
	if usage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok {
		if usage.Maxrss > 0 {
			result.MaxRSSBytes = uint64(usage.Maxrss) * 1024
		}
		result.MinorFaults = int64(usage.Minflt)
		result.MajorFaults = int64(usage.Majflt)
		result.VoluntarySwitches = int64(usage.Nvcsw)
		result.InvoluntarySwitches = int64(usage.Nivcsw)
	}
	return result
}

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
}

func runHook(ctx context.Context, plan plan, hook hook) hookResult {
	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(hook.TimeoutSeconds)*time.Second)
	defer cancel()
	environment, overrides, digest := commandEnvironment(plan.InheritEnvironment, plan.Environment, hook.Environment, nil)
	cmd := exec.CommandContext(hookCtx, hook.Command[0], hook.Command[1:]...)
	configureProcessGroup(cmd)
	cmd.Dir = plan.WorkingDirectory
	cmd.Env = environment
	stdout := newBoundedBuffer(maxCapturedOutputBytes)
	stderr := newBoundedBuffer(maxCapturedOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	started := time.Now()
	err := cmd.Run()
	result := hookResult{
		ID: hook.ID, Command: append([]string(nil), hook.Command...), Environment: overrides,
		EnvironmentSHA256: digest, WallNS: time.Since(started).Nanoseconds(), ExitCode: -1,
		TimedOut: errors.Is(hookCtx.Err(), context.DeadlineExceeded),
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if err != nil && result.ExitCode == 0 {
		result.ExitCode = -1
	}
	result.Stdout, result.StdoutTruncated = stdout.snapshot()
	result.Stderr, result.StderrTruncated = stderr.snapshot()
	return result
}

func classifyValidity(ledger []protocol.LedgerEntry) string {
	validity := validityValid
	for _, entry := range ledger {
		if entry.Severity != "error" {
			continue
		}
		if entry.Owner == "infrastructure" || entry.Owner == "harness" {
			return validityInfrastructure
		}
		validity = validityCandidate
	}
	return validity
}

type jsonlWriter struct {
	file    *os.File
	buffer  *bufio.Writer
	encoder *json.Encoder
	hash    hash.Hash
	rows    int
}

func newJSONLWriter(path string) (*jsonlWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create results directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create results file: %w", err)
	}
	hasher := sha256.New()
	buffer := bufio.NewWriterSize(io.MultiWriter(file, hasher), 128*1024)
	return &jsonlWriter{
		file: file, buffer: buffer, encoder: json.NewEncoder(buffer),
		hash: hasher,
	}, nil
}

func (writer *jsonlWriter) WriteBlock(rows []runResult) error {
	for _, row := range rows {
		if err := writer.encoder.Encode(row); err != nil {
			return fmt.Errorf("encode result row: %w", err)
		}
		writer.rows++
	}
	if err := writer.buffer.Flush(); err != nil {
		return fmt.Errorf("flush result block: %w", err)
	}
	if err := writer.file.Sync(); err != nil {
		return fmt.Errorf("sync result block: %w", err)
	}
	return nil
}

func (writer *jsonlWriter) Close() (string, int, error) {
	flushErr := writer.buffer.Flush()
	syncErr := writer.file.Sync()
	closeErr := writer.file.Close()
	digest := hex.EncodeToString(writer.hash.Sum(nil))
	return digest, writer.rows, errors.Join(flushErr, syncErr, closeErr)
}

func isWithin(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, fmt.Errorf("compare results and corpus paths: %w", err)
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}
