package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/betterleaks/betterleaks/sources"
)

const fileScanMetricsFDEnv = "BETTERLEAKS_FILESCAN_METRICS_FD"

func init() {
	flags := directoryCmd.Flags()
	flags.String("filescan-backend", "", "experimental filesystem content backend")
	flags.String("filescan-namespace", string(sources.FileScanNamespaceParallel), "experimental namespace backend")
	flags.String("filescan-read-mode", sources.FileScanReadModeRead, "buffered read mode")
	flags.Int("filescan-prefetch-kib", 0, "physical prefetch/direct/io_uring buffer size in KiB")
	flags.StringSlice("filescan-advice", nil, "kernel advice modes")
	flags.Int64("filescan-mmap-window-mib", 0, "mmap window size in MiB")
	flags.Int("filescan-queue-depth", 0, "global io_uring queue depth")
	flags.Bool("filescan-registered-buffers", false, "use registered io_uring buffers")
	flags.Int64("filescan-max-inflight-mib", 0, "global scanner-owned in-flight byte limit in MiB")
	flags.Int("filescan-active-files", 0, "maximum active files")
	flags.Int("filescan-detector-workers", 0, "detector worker count")
	flags.Bool("filescan-inline-detector", false, "retain production inline detector scheduling")
	flags.Int("filescan-walkers", 0, "directory walker count")
	flags.Bool("filescan-strict", false, "reject unsupported backends instead of falling back")

	for _, name := range []string{
		"filescan-backend",
		"filescan-namespace",
		"filescan-read-mode",
		"filescan-prefetch-kib",
		"filescan-advice",
		"filescan-mmap-window-mib",
		"filescan-queue-depth",
		"filescan-registered-buffers",
		"filescan-max-inflight-mib",
		"filescan-active-files",
		"filescan-detector-workers",
		"filescan-inline-detector",
		"filescan-walkers",
		"filescan-strict",
	} {
		_ = flags.MarkHidden(name)
	}
}

func fileScanConfigFromFlags(cmd *cobra.Command) (*sources.FileScanConfig, error) {
	backend, err := cmd.Flags().GetString("filescan-backend")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(backend) == "" {
		return nil, nil
	}
	namespace, err := cmd.Flags().GetString("filescan-namespace")
	if err != nil {
		return nil, err
	}
	readMode, err := cmd.Flags().GetString("filescan-read-mode")
	if err != nil {
		return nil, err
	}
	prefetchKiB, err := cmd.Flags().GetInt("filescan-prefetch-kib")
	if err != nil {
		return nil, err
	}
	advice, err := cmd.Flags().GetStringSlice("filescan-advice")
	if err != nil {
		return nil, err
	}
	mmapMiB, err := cmd.Flags().GetInt64("filescan-mmap-window-mib")
	if err != nil {
		return nil, err
	}
	queueDepth, err := cmd.Flags().GetInt("filescan-queue-depth")
	if err != nil {
		return nil, err
	}
	registered, err := cmd.Flags().GetBool("filescan-registered-buffers")
	if err != nil {
		return nil, err
	}
	maxInFlightMiB, err := cmd.Flags().GetInt64("filescan-max-inflight-mib")
	if err != nil {
		return nil, err
	}
	activeFiles, err := cmd.Flags().GetInt("filescan-active-files")
	if err != nil {
		return nil, err
	}
	detectorWorkers, err := cmd.Flags().GetInt("filescan-detector-workers")
	if err != nil {
		return nil, err
	}
	inlineDetector, err := cmd.Flags().GetBool("filescan-inline-detector")
	if err != nil {
		return nil, err
	}
	walkers, err := cmd.Flags().GetInt("filescan-walkers")
	if err != nil {
		return nil, err
	}
	strict, err := cmd.Flags().GetBool("filescan-strict")
	if err != nil {
		return nil, err
	}

	prefetchBytes, err := scaleFileScanBytes(int64(prefetchKiB), 1<<10, "prefetch")
	if err != nil {
		return nil, err
	}
	mmapWindowBytes, err := scaleFileScanBytes(mmapMiB, 1<<20, "mmap window")
	if err != nil {
		return nil, err
	}
	maxInFlightBytes, err := scaleFileScanBytes(maxInFlightMiB, 1<<20, "max in-flight")
	if err != nil {
		return nil, err
	}
	if prefetchBytes > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("filescan: prefetch size exceeds int range")
	}

	cfg := sources.FileScanConfig{
		Backend:           sources.FileScanBackend(backend),
		Namespace:         sources.FileScanNamespace(namespace),
		ReadMode:          readMode,
		PrefetchBytes:     int(prefetchBytes),
		Advice:            advice,
		MmapWindowBytes:   mmapWindowBytes,
		QueueDepth:        queueDepth,
		RegisteredBuffers: registered,
		MaxInFlightBytes:  maxInFlightBytes,
		ActiveFiles:       activeFiles,
		DetectorWorkers:   detectorWorkers,
		InlineDetector:    inlineDetector,
		Walkers:           walkers,
		Strict:            strict,
		Metrics:           sources.NewFileScanMetrics(),
	}.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func scaleFileScanBytes(value, scale int64, name string) (int64, error) {
	if value < 0 {
		return 0, fmt.Errorf("filescan: %s must not be negative", name)
	}
	if value != 0 && value > int64(^uint64(0)>>1)/scale {
		return 0, fmt.Errorf("filescan: %s size overflows int64", name)
	}
	return value * scale, nil
}

type fileScanOutputIdentity struct {
	Count          uint64 `json:"count"`
	CanonicalBytes uint64 `json:"canonical_bytes"`
	Digest         string `json:"digest"`
}

type fileScanOutputPhase struct {
	Operations   uint64            `json:"operations"`
	Bytes        uint64            `json:"bytes"`
	Errors       uint64            `json:"errors"`
	CumulativeNS uint64            `json:"cumulative_ns"`
	Histogram    map[string]uint64 `json:"histogram,omitempty"`
}

type fileScanOutputBackend struct {
	Files uint64 `json:"files"`
	Bytes uint64 `json:"bytes"`
}

type fileScanOutputResources struct {
	MaxActiveFiles              uint64 `json:"max_active_files,omitempty"`
	MaxFragmentQueueDepth       uint64 `json:"max_fragment_queue_depth,omitempty"`
	MaxInFlightBytes            uint64 `json:"max_in_flight_bytes,omitempty"`
	MaxMappedBytes              uint64 `json:"max_mapped_bytes,omitempty"`
	MaxPinnedBytes              uint64 `json:"max_pinned_bytes,omitempty"`
	MaxQueueDepth               uint64 `json:"max_queue_depth,omitempty"`
	MaxSubmittedBytes           uint64 `json:"max_submitted_bytes,omitempty"`
	MaxCompletedUnconsumedBytes uint64 `json:"max_completed_unconsumed_bytes,omitempty"`
	MaxReorderDepth             uint64 `json:"max_reorder_depth,omitempty"`
}

type fileScanMetricsPayload struct {
	SchemaVersion   int                              `json:"schema_version"`
	Candidate       string                           `json:"candidate,omitempty"`
	Targets         fileScanOutputIdentity           `json:"targets"`
	Fragments       fileScanOutputIdentity           `json:"fragments"`
	Findings        fileScanOutputIdentity           `json:"findings"`
	Errors          fileScanOutputIdentity           `json:"errors"`
	Fallbacks       fileScanOutputIdentity           `json:"fallbacks"`
	Phases          map[string]fileScanOutputPhase   `json:"phases,omitempty"`
	Backends        map[string]fileScanOutputBackend `json:"backends,omitempty"`
	FallbackReasons map[string]uint64                `json:"fallback_reasons,omitempty"`
	LedgerKinds     map[string]uint64                `json:"ledger_kinds,omitempty"`
	Resources       fileScanOutputResources          `json:"resources,omitempty"`
}

func writeFileScanMetrics(cfg *sources.FileScanConfig) error {
	if cfg == nil {
		return nil
	}
	fdText := strings.TrimSpace(os.Getenv(fileScanMetricsFDEnv))
	if fdText == "" {
		return nil
	}
	fd, err := strconv.Atoi(fdText)
	if err != nil || fd < 3 {
		return fmt.Errorf("%s must be an inherited descriptor >= 3", fileScanMetricsFDEnv)
	}
	file := os.NewFile(uintptr(fd), "betterleaks-filescan-metrics")
	if file == nil {
		return fmt.Errorf("open inherited filescan metrics descriptor %d", fd)
	}
	payload := makeFileScanMetricsPayload(*cfg, cfg.Metrics.Snapshot())
	if err := json.NewEncoder(file).Encode(payload); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func makeFileScanMetricsPayload(
	cfg sources.FileScanConfig,
	snapshot sources.FileScanSnapshot,
) fileScanMetricsPayload {
	identity := func(digest sources.FileScanDigestSnapshot) fileScanOutputIdentity {
		return fileScanOutputIdentity{
			Count:          digest.Count,
			CanonicalBytes: digest.CanonicalBytes,
			Digest:         digest.SHA256,
		}
	}
	phases := make(map[string]fileScanOutputPhase, len(snapshot.Phases))
	for name, phase := range snapshot.Phases {
		histogram := make(map[string]uint64)
		for bucket, count := range phase.LatencyLog2NS {
			if count != 0 {
				histogram[strconv.Itoa(bucket)] = count
			}
		}
		phases[name] = fileScanOutputPhase{
			Operations:   phase.Count,
			Bytes:        phase.Bytes,
			Errors:       phase.Errors,
			CumulativeNS: phase.Nanoseconds,
			Histogram:    histogram,
		}
	}
	backends := make(map[string]fileScanOutputBackend, len(snapshot.Backends))
	for name, backend := range snapshot.Backends {
		backends[name] = fileScanOutputBackend{Files: backend.Files, Bytes: backend.Bytes}
	}
	peak := func(name string) uint64 {
		value := snapshot.Resources[name].Peak
		if value <= 0 {
			return 0
		}
		return uint64(value)
	}
	return fileScanMetricsPayload{
		SchemaVersion:   sources.FileScanSnapshotSchemaVersion,
		Candidate:       string(cfg.Backend) + "/" + string(cfg.Namespace),
		Targets:         identity(snapshot.Digests.Targets),
		Fragments:       identity(snapshot.Digests.Fragments),
		Findings:        identity(snapshot.Digests.Findings),
		Errors:          identity(snapshot.Digests.Errors),
		Fallbacks:       identity(snapshot.Digests.Fallbacks),
		Phases:          phases,
		Backends:        backends,
		FallbackReasons: snapshot.FallbackReasons,
		LedgerKinds:     snapshot.LedgerKinds,
		Resources: fileScanOutputResources{
			MaxActiveFiles:              peak(sources.FileScanGaugeActiveFiles),
			MaxFragmentQueueDepth:       peak("fragment_queue_depth"),
			MaxInFlightBytes:            peak(sources.FileScanGaugeInFlightBytes),
			MaxMappedBytes:              peak(sources.FileScanGaugeMappedBytes),
			MaxPinnedBytes:              peak(sources.FileScanGaugePinnedBytes),
			MaxQueueDepth:               peak(sources.FileScanGaugeQueueDepth),
			MaxSubmittedBytes:           peak("submitted_bytes"),
			MaxCompletedUnconsumedBytes: peak("completed_unconsumed_bytes"),
			MaxReorderDepth:             peak("reorder_depth"),
		},
	}
}
