package sources

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileScanBackend identifies a content-reading implementation in the
// filesystem scanner tournament.
type FileScanBackend string

const (
	FileScanBackendBaseline    FileScanBackend = "baseline"
	FileScanBackendBuffered    FileScanBackend = "buffered"
	FileScanBackendMmap        FileScanBackend = "mmap"
	FileScanBackendDirect      FileScanBackend = "direct"
	FileScanBackendUring       FileScanBackend = "uring"
	FileScanBackendUringDirect FileScanBackend = "uring-direct"
)

// FileScanNamespace identifies a namespace-enumeration implementation.
type FileScanNamespace string

const (
	FileScanNamespaceParallel    FileScanNamespace = "parallel"
	FileScanNamespaceSerial      FileScanNamespace = "serial"
	FileScanNamespaceOpenat      FileScanNamespace = "openat"
	FileScanNamespaceUringOpenat FileScanNamespace = "uring-openat"
)

// FileScanAdvice is a normalized kernel-advice name. FileScanConfig retains
// []string for convenient flag decoding, while these constants prevent call
// sites from inventing spellings.
type FileScanAdvice string

const (
	FileScanAdviceFadvSequential FileScanAdvice = "fadv-sequential"
	FileScanAdviceFadvNoReuse    FileScanAdvice = "fadv-noreuse"
	FileScanAdviceFadvWillNeed   FileScanAdvice = "fadv-willneed"
	FileScanAdviceMadvSequential FileScanAdvice = "madv-sequential"
)

const (
	FileScanReadModeRead  = "read"
	FileScanReadModePread = "pread"

	defaultFileScanPrefetchBytes   = 512 * 1024
	defaultFileScanDirectReadBytes = 128 * 1024
	defaultFileScanMmapWindowBytes = 32 * 1024 * 1024
	defaultFileScanQueueDepth      = 32
	defaultFileScanInFlightBytes   = 64 * 1024 * 1024
	maxFileScanUringQueueDepth     = 32 * 1024
)

// FileScanConfig is the complete, reproducible scanner-tournament arm. Zero
// values select the current production behavior through Normalize.
type FileScanConfig struct {
	Backend           FileScanBackend   `json:"backend"`
	Namespace         FileScanNamespace `json:"namespace"`
	ReadMode          string            `json:"read_mode"`
	PrefetchBytes     int               `json:"prefetch_bytes"`
	Advice            []string          `json:"advice,omitempty"`
	MmapWindowBytes   int64             `json:"mmap_window_bytes"`
	QueueDepth        int               `json:"queue_depth"`
	RegisteredBuffers bool              `json:"registered_buffers"`
	MaxInFlightBytes  int64             `json:"max_in_flight_bytes"`
	ActiveFiles       int               `json:"active_files"`
	DetectorWorkers   int               `json:"detector_workers"`
	InlineDetector    bool              `json:"inline_detector"`
	Walkers           int               `json:"walkers"`
	Strict            bool              `json:"strict"`
	Metrics           *FileScanMetrics  `json:"-"`
}

// Normalize canonicalizes labels and fills safe, bounded defaults. It does not
// hide invalid negative values; Validate reports those after normalization.
func (c FileScanConfig) Normalize() FileScanConfig {
	n := c
	n.Backend = FileScanBackend(strings.ToLower(strings.TrimSpace(string(n.Backend))))
	if n.Backend == "" {
		n.Backend = FileScanBackendBaseline
	}
	n.Namespace = FileScanNamespace(strings.ToLower(strings.TrimSpace(string(n.Namespace))))
	if n.Namespace == "" {
		n.Namespace = FileScanNamespaceParallel
	}
	n.ReadMode = strings.ToLower(strings.TrimSpace(n.ReadMode))
	if n.ReadMode == "" {
		n.ReadMode = FileScanReadModeRead
	}

	if n.PrefetchBytes == 0 {
		switch n.Backend {
		case FileScanBackendBuffered, FileScanBackendUring, FileScanBackendUringDirect:
			n.PrefetchBytes = defaultFileScanPrefetchBytes
		case FileScanBackendDirect:
			n.PrefetchBytes = defaultFileScanDirectReadBytes
		}
	}
	if n.MmapWindowBytes == 0 && n.Backend == FileScanBackendMmap {
		n.MmapWindowBytes = defaultFileScanMmapWindowBytes
	}
	if n.QueueDepth == 0 && (isFileScanUringBackend(n.Backend) ||
		(n.Backend == FileScanBackendBaseline && n.Namespace == FileScanNamespaceUringOpenat)) {
		n.QueueDepth = defaultFileScanQueueDepth
	}
	if n.ActiveFiles == 0 {
		n.ActiveFiles = max(40, runtime.NumCPU()*2)
	}
	if n.DetectorWorkers == 0 {
		n.DetectorWorkers = runtime.GOMAXPROCS(0)
	}
	if n.Walkers == 0 {
		n.Walkers = runtime.NumCPU()
	}

	n.Advice = normalizeFileScanAdvice(n.Backend, n.Advice)
	if n.MaxInFlightBytes == 0 {
		n.MaxInFlightBytes = defaultFileScanInFlightBytes
		reserved := int64(0)
		switch n.Backend {
		case FileScanBackendBuffered:
			reserved = int64(n.PrefetchBytes)
		case FileScanBackendDirect:
			reserved = fileScanRoundedBufferBytes(max(n.PrefetchBytes, defaultBufferSize))
		case FileScanBackendMmap:
			reserved = n.MmapWindowBytes
		case FileScanBackendUring, FileScanBackendUringDirect:
			reserved = saturatingFileScanMul(
				int64(n.QueueDepth),
				fileScanRoundedBufferBytes(n.PrefetchBytes),
			)
		}
		required := saturatingFileScanAdd(reserved, int64(defaultBufferSize+4096))
		if required > n.MaxInFlightBytes {
			n.MaxInFlightBytes = required
		}
	}
	return n
}

func normalizeFileScanAdvice(backend FileScanBackend, advice []string) []string {
	if len(advice) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(advice))
	for _, raw := range advice {
		value := strings.ToLower(strings.TrimSpace(raw))
		value = strings.ReplaceAll(value, "_", "-")
		switch value {
		case "":
			continue
		case "sequential":
			if backend == FileScanBackendMmap {
				value = string(FileScanAdviceMadvSequential)
			} else {
				value = string(FileScanAdviceFadvSequential)
			}
		case "noreuse", "no-reuse":
			value = string(FileScanAdviceFadvNoReuse)
		case "willneed", "will-need":
			value = string(FileScanAdviceFadvWillNeed)
		}
		seen[value] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// Validate rejects unsupported tournament arms before a timed run starts.
func (c FileScanConfig) Validate() error {
	n := c.Normalize()

	switch n.Backend {
	case FileScanBackendBaseline, FileScanBackendBuffered, FileScanBackendMmap,
		FileScanBackendDirect, FileScanBackendUring, FileScanBackendUringDirect:
	default:
		return fmt.Errorf("filescan: unknown backend %q", n.Backend)
	}
	switch n.Namespace {
	case FileScanNamespaceParallel, FileScanNamespaceSerial, FileScanNamespaceOpenat,
		FileScanNamespaceUringOpenat:
	default:
		return fmt.Errorf("filescan: unknown namespace %q", n.Namespace)
	}
	if n.ReadMode != FileScanReadModeRead && n.ReadMode != FileScanReadModePread {
		return fmt.Errorf("filescan: unknown read mode %q", n.ReadMode)
	}
	if n.ReadMode == FileScanReadModePread && n.Backend != FileScanBackendBuffered {
		return fmt.Errorf("filescan: pread mode requires the buffered backend")
	}
	if n.PrefetchBytes < 0 {
		return fmt.Errorf("filescan: prefetch bytes must not be negative")
	}
	if n.MmapWindowBytes < 0 {
		return fmt.Errorf("filescan: mmap window bytes must not be negative")
	}
	if n.QueueDepth < 0 {
		return fmt.Errorf("filescan: queue depth must not be negative")
	}
	if n.MaxInFlightBytes <= 0 {
		return fmt.Errorf("filescan: max in-flight bytes must be positive")
	}
	if n.ActiveFiles <= 0 {
		return fmt.Errorf("filescan: active files must be positive")
	}
	if n.DetectorWorkers <= 0 {
		return fmt.Errorf("filescan: detector workers must be positive")
	}
	if n.Walkers <= 0 {
		return fmt.Errorf("filescan: walkers must be positive")
	}
	if int64(n.PrefetchBytes) > n.MaxInFlightBytes {
		return fmt.Errorf("filescan: prefetch bytes exceed max in-flight bytes")
	}
	if n.MmapWindowBytes > n.MaxInFlightBytes {
		return fmt.Errorf("filescan: mmap window bytes exceed max in-flight bytes")
	}

	isUring := isFileScanUringBackend(n.Backend)
	if n.Namespace == FileScanNamespaceUringOpenat &&
		n.Backend != FileScanBackendBaseline && !isUring {
		return fmt.Errorf("filescan: uring-openat namespace requires the baseline or a uring content backend")
	}
	usesUringQueue := isUring ||
		(n.Backend == FileScanBackendBaseline && n.Namespace == FileScanNamespaceUringOpenat)
	if usesUringQueue && n.QueueDepth > maxFileScanUringQueueDepth {
		return fmt.Errorf("filescan: uring queue depth exceeds Linux maximum %d", maxFileScanUringQueueDepth)
	}
	if isUring && uint64(n.PrefetchBytes) > uint64(^uint32(0)) {
		return fmt.Errorf("filescan: uring prefetch bytes exceed SQE length range")
	}
	switch n.Backend {
	case FileScanBackendBaseline:
		if n.PrefetchBytes != 0 || n.MmapWindowBytes != 0 || n.RegisteredBuffers ||
			(n.QueueDepth != 0 && n.Namespace != FileScanNamespaceUringOpenat) {
			return fmt.Errorf("filescan: baseline backend cannot use prefetch, mmap, or registered-buffer options; queue requires the uring-openat namespace")
		}
	case FileScanBackendBuffered:
		if n.PrefetchBytes == 0 {
			return fmt.Errorf("filescan: buffered backend requires positive prefetch bytes")
		}
		if n.MmapWindowBytes != 0 || n.QueueDepth != 0 || n.RegisteredBuffers {
			return fmt.Errorf("filescan: buffered backend cannot use mmap, queue, or registered-buffer options")
		}
	case FileScanBackendMmap:
		if n.MmapWindowBytes == 0 {
			return fmt.Errorf("filescan: mmap backend requires a positive window")
		}
		if n.MmapWindowBytes%int64(os.Getpagesize()) != 0 {
			return fmt.Errorf("filescan: mmap window must be a page-size multiple")
		}
		if n.MmapWindowBytes > int64(^uint(0)>>1) {
			return fmt.Errorf("filescan: mmap window exceeds platform int range")
		}
		if n.PrefetchBytes != 0 || n.QueueDepth != 0 || n.RegisteredBuffers {
			return fmt.Errorf("filescan: mmap backend cannot use prefetch, queue, or registered-buffer options")
		}
	case FileScanBackendDirect:
		if n.PrefetchBytes == 0 {
			return fmt.Errorf("filescan: direct backend requires positive read bytes")
		}
		if n.MmapWindowBytes != 0 || n.QueueDepth != 0 || n.RegisteredBuffers {
			return fmt.Errorf("filescan: direct backend cannot use mmap, queue, or registered-buffer options")
		}
	case FileScanBackendUring, FileScanBackendUringDirect:
		if n.PrefetchBytes == 0 || n.QueueDepth == 0 {
			return fmt.Errorf("filescan: uring backends require positive prefetch bytes and queue depth")
		}
		if n.MmapWindowBytes != 0 {
			return fmt.Errorf("filescan: uring backends cannot use mmap options")
		}
	}
	if n.RegisteredBuffers && !isUring {
		return fmt.Errorf("filescan: registered buffers require a uring backend")
	}
	reservedBytes := int64(0)
	switch n.Backend {
	case FileScanBackendBuffered:
		reservedBytes = int64(n.PrefetchBytes)
	case FileScanBackendDirect:
		reservedBytes = fileScanRoundedBufferBytes(max(n.PrefetchBytes, defaultBufferSize))
	case FileScanBackendMmap:
		reservedBytes = n.MmapWindowBytes
	case FileScanBackendUring, FileScanBackendUringDirect:
		reservedBytes = saturatingFileScanMul(
			int64(n.QueueDepth),
			fileScanRoundedBufferBytes(n.PrefetchBytes),
		)
	}
	minimumSession := int64(defaultBufferSize + 4096)
	if reservedBytes == maxFileScanInt64 || n.MaxInFlightBytes < minimumSession ||
		reservedBytes > n.MaxInFlightBytes-minimumSession {
		return fmt.Errorf(
			"filescan: backend storage and one framing session require more than max in-flight bytes",
		)
	}

	for _, advice := range n.Advice {
		switch FileScanAdvice(advice) {
		case FileScanAdviceFadvSequential, FileScanAdviceFadvNoReuse, FileScanAdviceFadvWillNeed:
			if n.Backend != FileScanBackendBuffered {
				return fmt.Errorf("filescan: %q requires the buffered backend", advice)
			}
		case FileScanAdviceMadvSequential:
			if n.Backend != FileScanBackendMmap {
				return fmt.Errorf("filescan: %q requires the mmap backend", advice)
			}
		default:
			return fmt.Errorf("filescan: unknown advice %q", advice)
		}
	}
	return nil
}

func isFileScanUringBackend(backend FileScanBackend) bool {
	return backend == FileScanBackendUring || backend == FileScanBackendUringDirect
}

// MemoryCost estimates the scanner-owned peak bytes for an arm: unchanged
// fragment buffers plus bounded backend transport or mapping storage. Kernel
// metadata and detector allocations are deliberately excluded.
func (c FileScanConfig) MemoryCost() int64 {
	n := c.Normalize()
	framing := saturatingFileScanMul(int64(max(n.ActiveFiles, 0)), int64(defaultBufferSize))

	var transport int64
	switch n.Backend {
	case FileScanBackendBuffered:
		transport = saturatingFileScanMul(int64(max(n.ActiveFiles, 0)), int64(max(n.PrefetchBytes, 0)))
	case FileScanBackendDirect:
		transport = saturatingFileScanMul(
			int64(max(n.ActiveFiles, 0)),
			fileScanRoundedBufferBytes(max(n.PrefetchBytes, defaultBufferSize)),
		)
	case FileScanBackendMmap:
		transport = saturatingFileScanMul(int64(max(n.ActiveFiles, 0)), max(n.MmapWindowBytes, 0))
	case FileScanBackendUring, FileScanBackendUringDirect:
		transport = saturatingFileScanMul(
			int64(max(n.QueueDepth, 0)),
			fileScanRoundedBufferBytes(n.PrefetchBytes),
		)
	}
	estimate := saturatingFileScanAdd(framing, transport)
	if n.MaxInFlightBytes > 0 && estimate > n.MaxInFlightBytes {
		return n.MaxInFlightBytes
	}
	return estimate
}

func fileScanRoundedBufferBytes(size int) int64 {
	if size <= 0 {
		return 0
	}
	value := int64(size)
	page := int64(os.Getpagesize())
	if value > maxFileScanInt64-(page-1) {
		return maxFileScanInt64
	}
	return ((value + page - 1) / page) * page
}

const maxFileScanInt64 = int64(^uint64(0) >> 1)

func saturatingFileScanMul(a, b int64) int64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	if a > maxFileScanInt64/b {
		return maxFileScanInt64
	}
	return a * b
}

func saturatingFileScanAdd(a, b int64) int64 {
	if a >= maxFileScanInt64-b {
		return maxFileScanInt64
	}
	return a + b
}

// FileScanPhase names are stable JSON keys shared by every implementation.
const (
	FileScanPhaseEnumeration   = "enumeration"
	FileScanPhaseMetadata      = "metadata"
	FileScanPhaseOpen          = "open"
	FileScanPhaseClose         = "close"
	FileScanPhaseReadWait      = "read_wait"
	FileScanPhaseFraming       = "framing"
	FileScanPhaseArchive       = "archive"
	FileScanPhaseDecompression = "decompression"
	FileScanPhaseDetector      = "detector"
)

// FileScan resource gauge names are stable JSON keys. Additional names are
// accepted so candidate-specific resources do not require schema changes.
const (
	FileScanGaugeActiveFiles   = "active_files"
	FileScanGaugeInFlightBytes = "in_flight_bytes"
	FileScanGaugeMappedBytes   = "mapped_bytes"
	FileScanGaugePinnedBytes   = "pinned_bytes"
	FileScanGaugeQueueDepth    = "queue_depth"
)

// FileScanLatencyBucketCount includes a zero-duration bucket followed by one
// bucket per positive time.Duration bit length: bucket 1 is 1 ns, bucket 2 is
// 2-3 ns, and bucket 63 covers 2^62 through the maximum time.Duration.
const FileScanLatencyBucketCount = 64

type fileScanPhaseAccumulator struct {
	Count         uint64
	Bytes         uint64
	Errors        uint64
	Nanoseconds   uint64
	LatencyLog2NS [FileScanLatencyBucketCount]uint64
}

// FileScanLocalMetrics accumulates phase timing without contending on the
// run-level collector. File processing can record read, framing, decompression,
// and detector phases concurrently, so its methods are safe for concurrent use.
type FileScanLocalMetrics struct {
	mu     sync.Mutex
	phases map[string]fileScanPhaseAccumulator // guarded by mu
}

// NewFileScanLocal creates per-file timing storage. Prefer metrics.NewLocal at
// call sites so a nil global metrics pointer incurs no allocation.
func NewFileScanLocal() *FileScanLocalMetrics {
	return &FileScanLocalMetrics{phases: make(map[string]fileScanPhaseAccumulator, 8)}
}

// NewLocal returns nil when metrics are disabled, allowing instrumentation to
// stay on the hot path without allocating or calling time.Now.
func (m *FileScanMetrics) NewLocal() *FileScanLocalMetrics {
	if m == nil {
		return nil
	}
	return NewFileScanLocal()
}

// RecordPhase adds one completed phase operation to local timing storage.
func (l *FileScanLocalMetrics) RecordPhase(phase string, bytes uint64, err error, elapsed time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.phases == nil {
		l.phases = make(map[string]fileScanPhaseAccumulator, 8)
	}
	phase = normalizeFileScanKey(phase)
	value := l.phases[phase]
	value.Count++
	value.Bytes += bytes
	if err != nil {
		value.Errors++
	}
	ns := elapsed.Nanoseconds()
	if ns < 0 {
		ns = 0
	}
	value.Nanoseconds += uint64(ns)
	value.LatencyLog2NS[fileScanLatencyBucket(ns)]++
	l.phases[phase] = value
	l.mu.Unlock()
}

// FileScanPhaseTimer avoids time.Now entirely when its local accumulator is
// nil. Done records exactly one operation.
type FileScanPhaseTimer struct {
	local *FileScanLocalMetrics
	phase string
	start time.Time
}

// StartPhase starts a local phase timer, or returns a no-op timer when metrics
// are disabled.
func (l *FileScanLocalMetrics) StartPhase(phase string) FileScanPhaseTimer {
	if l == nil {
		return FileScanPhaseTimer{}
	}
	return FileScanPhaseTimer{local: l, phase: phase, start: time.Now()}
}

// Done records the timer's elapsed duration, byte count, and error outcome.
func (t FileScanPhaseTimer) Done(bytes uint64, err error) {
	if t.local == nil {
		return
	}
	t.local.RecordPhase(t.phase, bytes, err, time.Since(t.start))
}

func fileScanLatencyBucket(ns int64) int {
	if ns <= 0 {
		return 0
	}
	return bits.Len64(uint64(ns))
}

// FileScanMetrics is the concurrency-safe run-level collector. The zero value
// is usable. Per-fragment canonical digests retain one 32-byte leaf per item so
// duplicate multiplicity can be preserved when Snapshot sorts the leaves.
type FileScanMetrics struct {
	mu sync.Mutex

	phases          map[string]fileScanPhaseAccumulator
	backends        map[string]FileScanBackendSnapshot
	gauges          map[string]FileScanGaugeSnapshot
	fallbackReasons map[string]uint64
	ledgerKinds     map[string]uint64

	targetHashes   [][sha256.Size]byte
	fragmentHashes [][sha256.Size]byte
	findingHashes  [][sha256.Size]byte
	errorHashes    [][sha256.Size]byte
	fallbackHashes [][sha256.Size]byte
	ledgerHashes   [][sha256.Size]byte

	targetCanonicalBytes   uint64
	fragmentCanonicalBytes uint64
	findingCanonicalBytes  uint64
	errorCanonicalBytes    uint64
	fallbackCanonicalBytes uint64
	ledgerCanonicalBytes   uint64
}

// NewFileScanMetrics creates an enabled run-level metrics collector.
func NewFileScanMetrics() *FileScanMetrics {
	return &FileScanMetrics{}
}

func (m *FileScanMetrics) ensureMapsLocked() {
	if m.phases == nil {
		m.phases = make(map[string]fileScanPhaseAccumulator)
	}
	if m.backends == nil {
		m.backends = make(map[string]FileScanBackendSnapshot)
	}
	if m.gauges == nil {
		m.gauges = make(map[string]FileScanGaugeSnapshot)
	}
	if m.fallbackReasons == nil {
		m.fallbackReasons = make(map[string]uint64)
	}
	if m.ledgerKinds == nil {
		m.ledgerKinds = make(map[string]uint64)
	}
}

// MergeLocal atomically detaches and consumes one batch of local timing
// counters. Concurrent records are either included in that batch or retained
// for the next merge, and the local accumulator remains reusable.
func (m *FileScanMetrics) MergeLocal(local *FileScanLocalMetrics) {
	if m == nil || local == nil {
		return
	}
	local.mu.Lock()
	if len(local.phases) == 0 {
		local.mu.Unlock()
		return
	}
	phases := local.phases
	local.phases = nil
	local.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureMapsLocked()
	for phase, add := range phases {
		value := m.phases[phase]
		value.Count += add.Count
		value.Bytes += add.Bytes
		value.Errors += add.Errors
		value.Nanoseconds += add.Nanoseconds
		for i := range value.LatencyLog2NS {
			value.LatencyLog2NS[i] += add.LatencyLog2NS[i]
		}
		m.phases[phase] = value
	}
}

// RecordPhase records one phase directly. Prefer local accumulation in file
// workers; this helper is intended for singleton run-level operations.
func (m *FileScanMetrics) RecordPhase(phase string, bytes uint64, err error, elapsed time.Duration) {
	if m == nil {
		return
	}
	local := NewFileScanLocal()
	local.RecordPhase(phase, bytes, err, elapsed)
	m.MergeLocal(local)
}

// RecordTarget adds the canonical relative path and symlink tuple to the
// target multiset. ScanTarget size is intentionally excluded from the identity.
func (m *FileScanMetrics) RecordTarget(root string, target ScanTarget) {
	if m == nil {
		return
	}
	hash, canonicalBytes := fileScanLeafHashAndBytes("target",
		[]byte(canonicalFileScanPath(root, target.Path)),
		[]byte(canonicalFileScanPath(root, target.Symlink)),
	)
	m.mu.Lock()
	m.targetHashes = append(m.targetHashes, hash)
	m.targetCanonicalBytes += canonicalBytes
	m.mu.Unlock()
}

// RecordFragment adds (relative path, symlink, StartLine, SHA-256(Raw)) to the
// fragment multiset. File fragments always populate Raw even when Bytes aliases
// the same content, so hashing Raw avoids double-counting the payload.
func (m *FileScanMetrics) RecordFragment(root string, fragment Fragment) {
	if m == nil {
		return
	}
	var line [8]byte
	binary.BigEndian.PutUint64(line[:], uint64(int64(fragment.StartLine)))
	rawHash := sha256.Sum256([]byte(fragment.Raw))
	hash, canonicalBytes := fileScanLeafHashAndBytes("fragment",
		[]byte(canonicalFileScanPath(root, fragment.Attr(AttrPath))),
		[]byte(canonicalFileScanPath(root, fragment.Attr(AttrFSSymlink))),
		line[:],
		rawHash[:],
	)
	m.mu.Lock()
	m.fragmentHashes = append(m.fragmentHashes, hash)
	m.fragmentCanonicalBytes += canonicalBytes
	m.mu.Unlock()
}

// RecordFinding hashes bytes already canonicalized by the detector/reporting
// layer. This collector deliberately does not reinterpret their encoding.
func (m *FileScanMetrics) RecordFinding(canonical []byte) {
	if m == nil {
		return
	}
	hash, canonicalBytes := fileScanLeafHashAndBytes("finding", canonical)
	m.mu.Lock()
	m.findingHashes = append(m.findingHashes, hash)
	m.findingCanonicalBytes += canonicalBytes
	m.mu.Unlock()
}

// RecordLedger records a canonical error/skip/fallback tuple without exposing
// paths or error text in the summary JSON. LedgerKinds retains aggregate counts.
func (m *FileScanMetrics) RecordLedger(kind, path, detail string) {
	if m == nil {
		return
	}
	kind = normalizeFileScanKey(kind)
	hash, canonicalBytes := fileScanLeafHashAndBytes("ledger",
		[]byte(kind),
		[]byte(canonicalFileScanPath("", path)),
		[]byte(strings.TrimSpace(detail)),
	)
	m.mu.Lock()
	m.ensureMapsLocked()
	m.ledgerKinds[kind]++
	m.errorHashes = append(m.errorHashes, hash)
	m.ledgerHashes = append(m.ledgerHashes, hash)
	m.errorCanonicalBytes += canonicalBytes
	m.ledgerCanonicalBytes += canonicalBytes
	m.mu.Unlock()
}

// RecordFallback records both the human-readable aggregate reason and the
// canonical ledger entry used by correctness gates.
func (m *FileScanMetrics) RecordFallback(reason string) {
	if m == nil {
		return
	}
	reason = normalizeFileScanKey(reason)
	ledgerHash, ledgerBytes := fileScanLeafHashAndBytes("ledger", []byte("fallback"), nil, []byte(reason))
	fallbackHash, fallbackBytes := fileScanLeafHashAndBytes("fallback", []byte(reason))
	m.mu.Lock()
	m.ensureMapsLocked()
	m.fallbackReasons[reason]++
	m.ledgerKinds["fallback"]++
	m.fallbackHashes = append(m.fallbackHashes, fallbackHash)
	m.ledgerHashes = append(m.ledgerHashes, ledgerHash)
	m.fallbackCanonicalBytes += fallbackBytes
	m.ledgerCanonicalBytes += ledgerBytes
	m.mu.Unlock()
}

// RecordBackend records one completed file and its logical bytes for the
// backend that actually served it, including fallbacks.
func (m *FileScanMetrics) RecordBackend(name string, bytes uint64) {
	if m == nil {
		return
	}
	name = normalizeFileScanKey(name)
	m.mu.Lock()
	m.ensureMapsLocked()
	value := m.backends[name]
	value.Files++
	value.Bytes += bytes
	m.backends[name] = value
	m.mu.Unlock()
}

// SetGauge replaces a resource gauge and advances its observed peak.
func (m *FileScanMetrics) SetGauge(name string, current int64) {
	if m == nil {
		return
	}
	if current < 0 {
		current = 0
	}
	name = normalizeFileScanKey(name)
	m.mu.Lock()
	m.ensureMapsLocked()
	value := m.gauges[name]
	value.Current = current
	if current > value.Peak {
		value.Peak = current
	}
	m.gauges[name] = value
	m.mu.Unlock()
}

// AddGauge applies a delta, clamps the current value at zero, advances the
// peak, and returns the new current value.
func (m *FileScanMetrics) AddGauge(name string, delta int64) int64 {
	if m == nil {
		return 0
	}
	name = normalizeFileScanKey(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureMapsLocked()
	value := m.gauges[name]
	if delta > 0 && value.Current > maxFileScanInt64-delta {
		value.Current = maxFileScanInt64
	} else if delta < 0 && (delta == -maxFileScanInt64-1 || value.Current < -delta) {
		value.Current = 0
	} else {
		value.Current += delta
	}
	if value.Current > value.Peak {
		value.Peak = value.Current
	}
	m.gauges[name] = value
	return value.Current
}

// Gauge returns the current and peak values for a resource gauge.
func (m *FileScanMetrics) Gauge(name string) FileScanGaugeSnapshot {
	if m == nil {
		return FileScanGaugeSnapshot{}
	}
	name = normalizeFileScanKey(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gauges[name]
}

func normalizeFileScanKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func canonicalFileScanPath(root, path string) string {
	if path == "" {
		return ""
	}
	cleanPath := filepath.Clean(path)
	if root != "" {
		absRoot, rootErr := filepath.Abs(filepath.Clean(root))
		absPath, pathErr := filepath.Abs(cleanPath)
		if rootErr == nil && pathErr == nil {
			if relative, err := filepath.Rel(absRoot, absPath); err == nil {
				cleanPath = relative
			}
		}
	}
	return filepath.ToSlash(filepath.Clean(cleanPath))
}

func fileScanLeafHash(domain string, fields ...[]byte) [sha256.Size]byte {
	hash, _ := fileScanLeafHashAndBytes(domain, fields...)
	return hash
}

func fileScanLeafHashAndBytes(domain string, fields ...[]byte) ([sha256.Size]byte, uint64) {
	h := sha256.New()
	fileScanWriteField(h, []byte(domain))
	canonicalBytes := uint64(8 + len(domain))
	for _, field := range fields {
		fileScanWriteField(h, field)
		canonicalBytes += uint64(8 + len(field))
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	return sum, canonicalBytes
}

type fileScanWriter interface {
	Write([]byte) (int, error)
}

func fileScanWriteField(w fileScanWriter, field []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(field)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(field)
}

// FileScanSnapshotSchemaVersion identifies the stable JSON snapshot schema.
const FileScanSnapshotSchemaVersion = 1

// FileScanPhaseSnapshot contains aggregate timing and outcome data for a phase.
type FileScanPhaseSnapshot struct {
	Count         uint64                             `json:"count"`
	Bytes         uint64                             `json:"bytes"`
	Errors        uint64                             `json:"errors"`
	Nanoseconds   uint64                             `json:"ns"`
	LatencyLog2NS [FileScanLatencyBucketCount]uint64 `json:"latency_log2_ns"`
}

// FileScanBackendSnapshot contains logical file and byte counts served by a backend.
type FileScanBackendSnapshot struct {
	Files uint64 `json:"files"`
	Bytes uint64 `json:"bytes"`
}

// FileScanGaugeSnapshot contains current and peak values for a resource.
type FileScanGaugeSnapshot struct {
	Current int64 `json:"current"`
	Peak    int64 `json:"peak"`
}

// FileScanDigestSnapshot identifies a canonical multiset and its multiplicity.
type FileScanDigestSnapshot struct {
	Count          uint64 `json:"count"`
	CanonicalBytes uint64 `json:"canonical_bytes"`
	SHA256         string `json:"sha256"`
}

// FileScanDigestsSnapshot contains correctness digests for every recorded domain.
type FileScanDigestsSnapshot struct {
	Targets   FileScanDigestSnapshot `json:"targets"`
	Fragments FileScanDigestSnapshot `json:"fragments"`
	Findings  FileScanDigestSnapshot `json:"findings"`
	Errors    FileScanDigestSnapshot `json:"errors"`
	Fallbacks FileScanDigestSnapshot `json:"fallbacks"`
	Ledger    FileScanDigestSnapshot `json:"ledger"`
}

// FileScanSnapshot is the versioned JSON artifact written by the benchmark
// harness. Maps contain aggregate labels only; correctness identities live in
// domain-separated multiset digests.
type FileScanSnapshot struct {
	SchemaVersion   int                                `json:"schema_version"`
	Phases          map[string]FileScanPhaseSnapshot   `json:"phases"`
	Backends        map[string]FileScanBackendSnapshot `json:"backends"`
	Resources       map[string]FileScanGaugeSnapshot   `json:"resources"`
	FallbackReasons map[string]uint64                  `json:"fallback_reasons"`
	LedgerKinds     map[string]uint64                  `json:"ledger_kinds"`
	Digests         FileScanDigestsSnapshot            `json:"digests"`
}

// Snapshot copies counters under lock, then sorts copied leaf hashes and
// computes digests without blocking scanner workers.
func (m *FileScanMetrics) Snapshot() FileScanSnapshot {
	snapshot := FileScanSnapshot{
		SchemaVersion:   FileScanSnapshotSchemaVersion,
		Phases:          make(map[string]FileScanPhaseSnapshot),
		Backends:        make(map[string]FileScanBackendSnapshot),
		Resources:       make(map[string]FileScanGaugeSnapshot),
		FallbackReasons: make(map[string]uint64),
		LedgerKinds:     make(map[string]uint64),
	}
	var targets, fragments, findings, scanErrors, fallbacks, ledger [][sha256.Size]byte
	var targetBytes, fragmentBytes, findingBytes, errorBytes, fallbackBytes, ledgerBytes uint64
	if m != nil {
		m.mu.Lock()
		for name, value := range m.phases {
			snapshot.Phases[name] = FileScanPhaseSnapshot(value)
		}
		for name, value := range m.backends {
			snapshot.Backends[name] = value
		}
		for name, value := range m.gauges {
			snapshot.Resources[name] = value
		}
		for reason, count := range m.fallbackReasons {
			snapshot.FallbackReasons[reason] = count
		}
		for kind, count := range m.ledgerKinds {
			snapshot.LedgerKinds[kind] = count
		}
		targets = append(targets, m.targetHashes...)
		fragments = append(fragments, m.fragmentHashes...)
		findings = append(findings, m.findingHashes...)
		scanErrors = append(scanErrors, m.errorHashes...)
		fallbacks = append(fallbacks, m.fallbackHashes...)
		ledger = append(ledger, m.ledgerHashes...)
		targetBytes = m.targetCanonicalBytes
		fragmentBytes = m.fragmentCanonicalBytes
		findingBytes = m.findingCanonicalBytes
		errorBytes = m.errorCanonicalBytes
		fallbackBytes = m.fallbackCanonicalBytes
		ledgerBytes = m.ledgerCanonicalBytes
		m.mu.Unlock()
	}
	snapshot.Digests = FileScanDigestsSnapshot{
		Targets:   fileScanMultisetDigest("targets", targets, targetBytes),
		Fragments: fileScanMultisetDigest("fragments", fragments, fragmentBytes),
		Findings:  fileScanMultisetDigest("findings", findings, findingBytes),
		Errors:    fileScanMultisetDigest("errors", scanErrors, errorBytes),
		Fallbacks: fileScanMultisetDigest("fallbacks", fallbacks, fallbackBytes),
		Ledger:    fileScanMultisetDigest("ledger", ledger, ledgerBytes),
	}
	return snapshot
}

func fileScanMultisetDigest(
	domain string,
	leaves [][sha256.Size]byte,
	canonicalBytes uint64,
) FileScanDigestSnapshot {
	sort.Slice(leaves, func(i, j int) bool {
		return bytes.Compare(leaves[i][:], leaves[j][:]) < 0
	})
	h := sha256.New()
	fileScanWriteField(h, []byte(domain))
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(leaves)))
	_, _ = h.Write(count[:])
	for _, leaf := range leaves {
		_, _ = h.Write(leaf[:])
	}
	return FileScanDigestSnapshot{
		Count:          uint64(len(leaves)),
		CanonicalBytes: canonicalBytes,
		SHA256:         hex.EncodeToString(h.Sum(nil)),
	}
}
