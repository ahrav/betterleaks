// Package protocol defines the versioned JSON exchanged by the scanner and
// the filesystem benchmark harness.
package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// SchemaVersion is the current filescan protocol version.
const SchemaVersion = 1

// OutputIdentity is an order-independent identity for one scanner output
// stream. CanonicalBytes is the byte length before per-record hashing.
type OutputIdentity struct {
	Count          uint64 `json:"count"`
	CanonicalBytes uint64 `json:"canonical_bytes"`
	Digest         string `json:"digest"`
}

// ExpectedOutputs contains the correctness oracle frozen in a corpus
// manifest. A stream with an empty digest has not been frozen yet.
type ExpectedOutputs struct {
	Targets   OutputIdentity `json:"targets"`
	Fragments OutputIdentity `json:"fragments"`
	Findings  OutputIdentity `json:"findings"`
	Errors    OutputIdentity `json:"errors"`
	Fallbacks OutputIdentity `json:"fallbacks"`
}

// LedgerEntry records a validity fact without silently discarding a run.
type LedgerEntry struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Owner    string `json:"owner"`
	Message  string `json:"message,omitempty"`
}

// PhaseMetric is an aggregate for one scanner phase. CumulativeNS is summed
// across concurrent operations and therefore is not additive wall time.
type PhaseMetric struct {
	Operations   uint64            `json:"operations"`
	Bytes        uint64            `json:"bytes"`
	Errors       uint64            `json:"errors"`
	CumulativeNS uint64            `json:"cumulative_ns"`
	Histogram    map[string]uint64 `json:"histogram,omitempty"`
}

// ScannerResources carries scanner-side measurements unavailable to the
// external procfs sampler.
type ScannerResources struct {
	MaxActiveFiles              uint64  `json:"max_active_files,omitempty"`
	MaxFragmentQueueDepth       uint64  `json:"max_fragment_queue_depth,omitempty"`
	MaxInFlightBytes            uint64  `json:"max_in_flight_bytes,omitempty"`
	MaxMappedBytes              uint64  `json:"max_mapped_bytes,omitempty"`
	MaxPinnedBytes              uint64  `json:"max_pinned_bytes,omitempty"`
	MaxQueueDepth               uint64  `json:"max_queue_depth,omitempty"`
	MaxSubmittedBytes           uint64  `json:"max_submitted_bytes,omitempty"`
	MaxCompletedUnconsumedBytes uint64  `json:"max_completed_unconsumed_bytes,omitempty"`
	MaxReorderDepth             uint64  `json:"max_reorder_depth,omitempty"`
	PageCacheHarmBytes          *uint64 `json:"page_cache_harm_bytes,omitempty"`
}

// BackendUsage records the logical files and bytes actually served by a
// backend. Intentional random-access routing and true fallbacks therefore stay
// visible without becoming part of the correctness identity.
type BackendUsage struct {
	Files uint64 `json:"files"`
	Bytes uint64 `json:"bytes"`
}

// ScannerMetrics is the single JSON object optionally emitted on the file
// descriptor named by BETTERLEAKS_FILESCAN_METRICS_FD.
type ScannerMetrics struct {
	SchemaVersion   int                     `json:"schema_version"`
	Candidate       string                  `json:"candidate,omitempty"`
	Targets         OutputIdentity          `json:"targets"`
	Fragments       OutputIdentity          `json:"fragments"`
	Findings        OutputIdentity          `json:"findings"`
	Errors          OutputIdentity          `json:"errors"`
	Fallbacks       OutputIdentity          `json:"fallbacks"`
	Phases          map[string]PhaseMetric  `json:"phases,omitempty"`
	Backends        map[string]BackendUsage `json:"backends,omitempty"`
	FallbackReasons map[string]uint64       `json:"fallback_reasons,omitempty"`
	LedgerKinds     map[string]uint64       `json:"ledger_kinds,omitempty"`
	Resources       ScannerResources        `json:"resources,omitempty"`
	Validity        []LedgerEntry           `json:"validity_ledger,omitempty"`
}

// Outputs returns the correctness-relevant part of m.
func (m ScannerMetrics) Outputs() ExpectedOutputs {
	return ExpectedOutputs{
		Targets: m.Targets, Fragments: m.Fragments, Findings: m.Findings,
		Errors: m.Errors, Fallbacks: m.Fallbacks,
	}
}

// DecodeScannerMetrics strictly decodes exactly one scanner metrics object.
func DecodeScannerMetrics(data []byte) (ScannerMetrics, error) {
	var metrics ScannerMetrics
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metrics); err != nil {
		return ScannerMetrics{}, fmt.Errorf("decode scanner metrics: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return ScannerMetrics{}, err
	}
	if err := metrics.Validate(); err != nil {
		return ScannerMetrics{}, err
	}
	return metrics, nil
}

// Validate checks the versioned scanner metrics contract.
func (m ScannerMetrics) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("scanner metrics schema_version %d, want %d", m.SchemaVersion, SchemaVersion)
	}
	outputs := []struct {
		name string
		id   OutputIdentity
	}{
		{"targets", m.Targets}, {"fragments", m.Fragments},
		{"findings", m.Findings}, {"errors", m.Errors},
		{"fallbacks", m.Fallbacks},
	}
	for _, output := range outputs {
		if err := validateDigest(output.name, output.id.Digest, true); err != nil {
			return err
		}
	}
	for i, entry := range m.Validity {
		if entry.Code == "" {
			return fmt.Errorf("validity_ledger[%d].code is required", i)
		}
		switch entry.Severity {
		case "info", "warning", "error":
		default:
			return fmt.Errorf("validity_ledger[%d].severity %q is invalid", i, entry.Severity)
		}
		if entry.Owner != "candidate" {
			return fmt.Errorf("validity_ledger[%d].owner must be candidate", i)
		}
	}
	return nil
}

// ValidateExpected checks every populated oracle digest.
func ValidateExpected(expected ExpectedOutputs) error {
	outputs := []struct {
		name string
		id   OutputIdentity
	}{
		{"targets", expected.Targets}, {"fragments", expected.Fragments},
		{"findings", expected.Findings}, {"errors", expected.Errors},
		{"fallbacks", expected.Fallbacks},
	}
	populated := 0
	for _, output := range outputs {
		if err := validateDigest(output.name, output.id.Digest, false); err != nil {
			return err
		}
		if output.id.Digest == "" && (output.id.Count != 0 || output.id.CanonicalBytes != 0) {
			return fmt.Errorf("%s has counts but no frozen digest", output.name)
		}
		if output.id.Digest != "" {
			populated++
		}
	}
	if populated != 0 && populated != len(outputs) {
		return fmt.Errorf("expected outputs must freeze all five identities together")
	}
	return nil
}

// CompareExpected returns candidate-owned error ledger entries for every
// populated oracle stream that differs from the observed stream.
func CompareExpected(expected, observed ExpectedOutputs) []LedgerEntry {
	type pair struct {
		name     string
		expected OutputIdentity
		observed OutputIdentity
	}
	pairs := []pair{
		{"targets", expected.Targets, observed.Targets},
		{"fragments", expected.Fragments, observed.Fragments},
		{"findings", expected.Findings, observed.Findings},
		{"errors", expected.Errors, observed.Errors},
		{"fallbacks", expected.Fallbacks, observed.Fallbacks},
	}
	var ledger []LedgerEntry
	for _, item := range pairs {
		if item.expected.Digest == "" {
			continue
		}
		if item.expected == item.observed {
			continue
		}
		ledger = append(ledger, LedgerEntry{
			Code:     item.name + "_identity_mismatch",
			Severity: "error",
			Owner:    "candidate",
			Message: fmt.Sprintf(
				"observed count=%d canonical_bytes=%d digest=%s; expected count=%d canonical_bytes=%d digest=%s",
				item.observed.Count, item.observed.CanonicalBytes, item.observed.Digest,
				item.expected.Count, item.expected.CanonicalBytes, item.expected.Digest,
			),
		})
	}
	return ledger
}

func validateDigest(name, digest string, required bool) error {
	if digest == "" && !required {
		return nil
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("%s digest must be 64 hexadecimal characters", name)
	}
	if digest != strings.ToLower(digest) {
		return fmt.Errorf("%s digest must use lowercase hexadecimal", name)
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode scanner metrics: trailing JSON value")
		}
		return fmt.Errorf("decode scanner metrics trailing data: %w", err)
	}
	return nil
}
