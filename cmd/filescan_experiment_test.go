package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/betterleaks/betterleaks/sources"
)

func TestFileScanMetricsPayloadMatchesFlatHarnessProtocol(t *testing.T) {
	metrics := sources.NewFileScanMetrics()
	root := t.TempDir()
	metrics.RecordTarget(root, sources.ScanTarget{Path: root + "/file.txt"})
	metrics.RecordFragment(root, sources.Fragment{
		Raw:        "content",
		StartLine:  1,
		Attributes: map[string]string{sources.AttrPath: root + "/file.txt"},
	})
	metrics.RecordFinding([]byte(`{"rule":"test"}`))
	metrics.RecordLedger("read_error", root+"/file.txt", "injected")
	metrics.RecordFallback("unsupported")
	metrics.RecordPhase(sources.FileScanPhaseReadWait, 7, nil, time.Microsecond)
	metrics.SetGauge(sources.FileScanGaugeActiveFiles, 3)
	metrics.SetGauge(sources.FileScanGaugeInFlightBytes, 4096)
	metrics.SetGauge(sources.FileScanGaugeQueueDepth, 5)
	metrics.RecordBackend("buffered", 7)

	cfg := (sources.FileScanConfig{
		Backend: sources.FileScanBackendBuffered, Namespace: sources.FileScanNamespaceOpenat,
		Metrics: metrics,
	}).Normalize()
	payload := makeFileScanMetricsPayload(cfg, metrics.Snapshot())
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"schema_version", "candidate", "targets", "fragments", "findings",
		"errors", "fallbacks", "phases", "backends", "fallback_reasons",
		"ledger_kinds", "resources",
	} {
		if _, ok := object[required]; !ok {
			t.Fatalf("payload omitted %q: %s", required, encoded)
		}
	}
	for _, nested := range []string{"config", "metrics", "wall_ns", "finding_count", "scan_error_count"} {
		if _, ok := object[nested]; ok {
			t.Fatalf("payload retained obsolete nested field %q: %s", nested, encoded)
		}
	}
	if payload.Candidate != "buffered/openat" {
		t.Fatalf("candidate = %q", payload.Candidate)
	}
	for name, identity := range map[string]fileScanOutputIdentity{
		"targets": payload.Targets, "fragments": payload.Fragments,
		"findings": payload.Findings, "errors": payload.Errors,
		"fallbacks": payload.Fallbacks,
	} {
		if identity.Count != 1 || identity.CanonicalBytes == 0 || len(identity.Digest) != 64 {
			t.Fatalf("%s identity = %+v", name, identity)
		}
	}
	if payload.Resources.MaxActiveFiles != 3 || payload.Resources.MaxInFlightBytes != 4096 ||
		payload.Resources.MaxQueueDepth != 5 {
		t.Fatalf("resources = %+v", payload.Resources)
	}
	if got := payload.Backends["buffered"]; got.Files != 1 || got.Bytes != 7 {
		t.Fatalf("backends = %+v", payload.Backends)
	}
	if payload.FallbackReasons["unsupported"] != 1 || payload.LedgerKinds["read_error"] != 1 {
		t.Fatalf("fallback reasons / ledger kinds = %+v / %+v", payload.FallbackReasons, payload.LedgerKinds)
	}
	phase := payload.Phases[sources.FileScanPhaseReadWait]
	if phase.Operations != 1 || phase.Bytes != 7 || phase.CumulativeNS != 1000 {
		t.Fatalf("phase = %+v", phase)
	}
}
