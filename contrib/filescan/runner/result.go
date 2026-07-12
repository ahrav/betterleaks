package main

import (
	"time"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/corpus"
	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

const resultSchemaVersion = 1

const (
	validityValid          = "valid"
	validityCandidate      = "invalid_candidate"
	validityInfrastructure = "invalid_infrastructure"
)

type runResult struct {
	SchemaVersion        int                      `json:"schema_version"`
	StudyID              string                   `json:"study_id"`
	Phase                string                   `json:"phase"`
	PlanSHA256           string                   `json:"plan_sha256"`
	BinarySHA256         string                   `json:"binary_sha256"`
	Artifacts            []artifact               `json:"artifacts"`
	ManifestSHA256       string                   `json:"manifest_sha256"`
	StratumID            string                   `json:"stratum_id"`
	WorkloadClass        string                   `json:"workload_class"`
	EvidenceRole         string                   `json:"evidence_role"`
	CacheState           string                   `json:"cache_state"`
	AllowedExitCodes     []int                    `json:"allowed_exit_codes"`
	BlockIndex           int                      `json:"block_index"`
	BlockAttempt         int                      `json:"block_attempt"`
	SequenceID           int                      `json:"sequence_id"`
	SequencePosition     int                      `json:"sequence_position"`
	CandidateID          string                   `json:"candidate_id"`
	MetricsCandidate     string                   `json:"metrics_candidate_expected,omitempty"`
	Started              bool                     `json:"started"`
	StartUTC             time.Time                `json:"start_utc,omitzero"`
	EndUTC               time.Time                `json:"end_utc,omitzero"`
	WallNS               int64                    `json:"wall_ns,omitempty"`
	Command              []string                 `json:"command"`
	WorkingDirectory     string                   `json:"working_directory"`
	Environment          map[string]string        `json:"environment_overrides"`
	EnvironmentSHA256    string                   `json:"environment_sha256"`
	Exit                 exitResult               `json:"exit"`
	Rusage               waitResourceUsage        `json:"wait_resource_usage"`
	Procfs               procResourceSummary      `json:"procfs"`
	Host                 hostTelemetry            `json:"host"`
	Hooks                []hookResult             `json:"hooks,omitempty"`
	LiveGuardBefore      *corpus.VerifyResult     `json:"live_guard_before,omitempty"`
	LiveGuardAfter       *corpus.VerifyResult     `json:"live_guard_after,omitempty"`
	ScannerMetrics       *protocol.ScannerMetrics `json:"scanner_metrics,omitempty"`
	ScannerMetricsSHA256 string                   `json:"scanner_metrics_sha256,omitempty"`
	Stdout               string                   `json:"stdout,omitempty"`
	StdoutTruncated      bool                     `json:"stdout_truncated,omitempty"`
	Stderr               string                   `json:"stderr,omitempty"`
	StderrTruncated      bool                     `json:"stderr_truncated,omitempty"`
	Validity             string                   `json:"validity"`
	BlockValidity        string                   `json:"block_validity"`
	ValidityLedger       []protocol.LedgerEntry   `json:"validity_ledger,omitempty"`
}

type exitResult struct {
	Code      int    `json:"code"`
	Signal    string `json:"signal,omitempty"`
	TimedOut  bool   `json:"timed_out"`
	WaitError string `json:"wait_error,omitempty"`
}

type waitResourceUsage struct {
	Scope               string `json:"scope"`
	UserCPU_NS          int64  `json:"user_cpu_ns"`
	SystemCPU_NS        int64  `json:"system_cpu_ns"`
	MaxRSSBytes         uint64 `json:"max_rss_bytes"`
	MinorFaults         int64  `json:"minor_faults"`
	MajorFaults         int64  `json:"major_faults"`
	VoluntarySwitches   int64  `json:"voluntary_context_switches"`
	InvoluntarySwitches int64  `json:"involuntary_context_switches"`
}

type hookResult struct {
	ID                string            `json:"id"`
	Command           []string          `json:"command"`
	Environment       map[string]string `json:"environment_overrides,omitempty"`
	EnvironmentSHA256 string            `json:"environment_sha256"`
	WallNS            int64             `json:"wall_ns"`
	ExitCode          int               `json:"exit_code"`
	TimedOut          bool              `json:"timed_out"`
	Stdout            string            `json:"stdout,omitempty"`
	StdoutTruncated   bool              `json:"stdout_truncated,omitempty"`
	Stderr            string            `json:"stderr,omitempty"`
	StderrTruncated   bool              `json:"stderr_truncated,omitempty"`
}

type runSummary struct {
	SchemaVersion                   int    `json:"schema_version"`
	StudyID                         string `json:"study_id"`
	PlanSHA256                      string `json:"plan_sha256"`
	ResultsPath                     string `json:"results_path"`
	ResultsSHA256                   string `json:"results_sha256"`
	Rows                            int    `json:"rows"`
	InfrastructureReplacementBlocks int    `json:"infrastructure_replacement_blocks"`
}
