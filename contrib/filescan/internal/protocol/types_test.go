package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func validMetrics() ScannerMetrics {
	identity := OutputIdentity{Count: 3, CanonicalBytes: 42, Digest: testDigest}
	return ScannerMetrics{
		SchemaVersion:   SchemaVersion,
		Targets:         identity,
		Fragments:       identity,
		Findings:        identity,
		Errors:          identity,
		Fallbacks:       identity,
		Backends:        map[string]BackendUsage{"buffered": {Files: 3, Bytes: 42}},
		FallbackReasons: map[string]uint64{"unsupported": 1},
		LedgerKinds:     map[string]uint64{"fallback": 1},
		Validity: []LedgerEntry{{
			Code: "fallback_observed", Severity: "warning", Owner: "candidate",
		}},
	}
}

func TestDecodeScannerMetricsStrict(t *testing.T) {
	data, err := json.Marshal(validMetrics())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeScannerMetrics(data); err != nil {
		t.Fatalf("decode valid metrics: %v", err)
	}

	withUnknown := strings.TrimSuffix(string(data), "}") + `,"unknown":true}`
	if _, err := DecodeScannerMetrics([]byte(withUnknown)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	if _, err := DecodeScannerMetrics(append(data, []byte(" {}")...)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
}

func TestScannerMetricsValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ScannerMetrics)
	}{
		{"schema", func(metrics *ScannerMetrics) { metrics.SchemaVersion++ }},
		{"digest", func(metrics *ScannerMetrics) { metrics.Targets.Digest = "not-a-digest" }},
		{"ledger-code", func(metrics *ScannerMetrics) { metrics.Validity[0].Code = "" }},
		{"ledger-severity", func(metrics *ScannerMetrics) { metrics.Validity[0].Severity = "fatal" }},
		{"ledger-owner", func(metrics *ScannerMetrics) { metrics.Validity[0].Owner = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics := validMetrics()
			test.mutate(&metrics)
			if err := metrics.Validate(); err == nil {
				t.Fatal("invalid metrics were accepted")
			}
		})
	}
}

func TestCompareExpected(t *testing.T) {
	expected := validMetrics().Outputs()
	observed := expected
	observed.Fragments.Count++
	observed.Errors.Digest = strings.Repeat("a", 64)
	ledger := CompareExpected(expected, observed)
	if len(ledger) != 2 {
		t.Fatalf("got %d mismatches, want 2: %#v", len(ledger), ledger)
	}
	for _, entry := range ledger {
		if entry.Severity != "error" || entry.Owner != "candidate" {
			t.Fatalf("mismatch ownership: %#v", entry)
		}
	}

	expected.Targets = OutputIdentity{}
	observed.Targets = OutputIdentity{Count: 99, Digest: testDigest}
	if got := CompareExpected(expected, observed); len(got) != 2 {
		t.Fatalf("unfrozen output affected comparison: %#v", got)
	}
}

func TestValidateExpectedRejectsPartiallyFrozenIdentity(t *testing.T) {
	expected := ExpectedOutputs{Targets: OutputIdentity{Count: 1}}
	if err := ValidateExpected(expected); err == nil {
		t.Fatal("counts without a frozen digest were accepted")
	}
	expected.Targets = OutputIdentity{Digest: strings.ToUpper(testDigest)}
	if err := ValidateExpected(expected); err == nil {
		t.Fatal("noncanonical uppercase digest was accepted")
	}
	expected.Targets = OutputIdentity{Digest: testDigest}
	if err := ValidateExpected(expected); err == nil {
		t.Fatal("partially frozen output set was accepted")
	}
}
