// Package corpus creates and verifies immutable filesystem benchmark corpus
// manifests.
package corpus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/betterleaks/betterleaks/contrib/filescan/internal/protocol"
)

// ManifestSchemaVersion is the current corpus manifest format.
const ManifestSchemaVersion = 1

// Manifest freezes both a portable tree identity and a host-local live guard.
type Manifest struct {
	SchemaVersion   int                      `json:"schema_version"`
	ID              string                   `json:"id"`
	WorkloadClass   string                   `json:"workload_class"`
	Role            string                   `json:"role"`
	Root            string                   `json:"root"`
	CreatedUTC      time.Time                `json:"created_utc"`
	Source          Source                   `json:"source"`
	EntriesFile     string                   `json:"entries_file"`
	EntriesSHA256   string                   `json:"entries_sha256"`
	PortableDigest  string                   `json:"portable_content_digest"`
	LiveGuardDigest string                   `json:"live_guard_digest"`
	Summary         Summary                  `json:"summary"`
	ExpectedOutputs protocol.ExpectedOutputs `json:"expected_outputs"`
	Oracle          *Oracle                  `json:"oracle,omitempty"`
}

// Source records how the corpus can be reconstructed.
type Source struct {
	Origin   string   `json:"origin"`
	Revision string   `json:"revision"`
	License  string   `json:"license,omitempty"`
	Objects  []string `json:"objects,omitempty"`
}

// Oracle records when and from which scanner identity the expected outputs
// were frozen.
type Oracle struct {
	FrozenUTC time.Time `json:"frozen_utc"`
	Candidate string    `json:"candidate,omitempty"`
}

// Summary describes the filesystem shape independently of scanner output.
type Summary struct {
	Entries           uint64            `json:"entries"`
	RegularFiles      uint64            `json:"regular_files"`
	Directories       uint64            `json:"directories"`
	Symlinks          uint64            `json:"symlinks"`
	OtherEntries      uint64            `json:"other_entries"`
	ManifestErrors    uint64            `json:"manifest_errors"`
	LogicalBytes      uint64            `json:"logical_bytes"`
	PhysicalBytes     uint64            `json:"physical_bytes"`
	SparseFiles       uint64            `json:"sparse_files"`
	MaxDepth          int               `json:"max_depth"`
	MaxFanout         uint64            `json:"max_fanout"`
	FileSizeBins      map[string]uint64 `json:"file_size_bins"`
	FileExtensions    map[string]uint64 `json:"file_extensions"`
	ArchiveExtensions map[string]uint64 `json:"archive_extensions"`
	ErrorCategories   map[string]uint64 `json:"error_categories"`
}

// CreateOptions configures manifest creation.
type CreateOptions struct {
	Root          string
	Output        string
	ID            string
	WorkloadClass string
	Role          string
	Source        Source
	Force         bool
}

// VerifyResult is emitted by manifest verification and can be persisted as a
// validity artifact.
type VerifyResult struct {
	SchemaVersion       int                    `json:"schema_version"`
	ManifestPath        string                 `json:"manifest_path"`
	Root                string                 `json:"root"`
	Mode                string                 `json:"mode"`
	Valid               bool                   `json:"valid"`
	ExpectedPortable    string                 `json:"expected_portable_content_digest,omitempty"`
	ObservedPortable    string                 `json:"observed_portable_content_digest,omitempty"`
	ExpectedLiveGuard   string                 `json:"expected_live_guard_digest,omitempty"`
	ObservedLiveGuard   string                 `json:"observed_live_guard_digest,omitempty"`
	ExpectedEntriesHash string                 `json:"expected_entries_sha256,omitempty"`
	ObservedEntriesHash string                 `json:"observed_entries_sha256,omitempty"`
	ObservedSummary     *Summary               `json:"observed_summary,omitempty"`
	Validity            []protocol.LedgerEntry `json:"validity_ledger,omitempty"`
}

// Create scans a corpus and atomically writes its manifest and canonical entry
// sidecar. Output must be outside Root so creating the manifest cannot mutate
// the corpus being identified.
func Create(ctx context.Context, options CreateOptions) (*Manifest, error) {
	if options.Root == "" {
		return nil, errors.New("corpus root is required")
	}
	if options.Output == "" {
		return nil, errors.New("manifest output is required")
	}
	if options.ID == "" {
		return nil, errors.New("manifest id is required")
	}
	if options.WorkloadClass == "" {
		return nil, errors.New("workload class is required")
	}
	if !validRole(options.Role) {
		return nil, fmt.Errorf("manifest role %q is invalid", options.Role)
	}
	if options.Source.Origin == "" || options.Source.Revision == "" {
		return nil, errors.New("source origin and revision are required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve corpus root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve corpus root symlinks: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat corpus root: %w", err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("corpus root %q is not a directory", root)
	}
	output, err := filepath.Abs(options.Output)
	if err != nil {
		return nil, fmt.Errorf("resolve manifest output: %w", err)
	}
	outputDirectory, err := resolveAllowMissing(filepath.Dir(output))
	if err != nil {
		return nil, fmt.Errorf("resolve manifest output directory symlinks: %w", err)
	}
	output = filepath.Join(outputDirectory, filepath.Base(output))
	inside, err := pathWithin(root, output)
	if err != nil {
		return nil, err
	}
	if inside {
		return nil, errors.New("manifest output must be outside the corpus root")
	}
	if err := os.MkdirAll(outputDirectory, 0o755); err != nil {
		return nil, fmt.Errorf("create manifest output directory: %w", err)
	}
	if !options.Force {
		if _, err := os.Lstat(output); err == nil {
			return nil, fmt.Errorf("output %q already exists", output)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("check output %q: %w", output, err)
		}
	}

	entriesTemp, err := os.CreateTemp(filepath.Dir(output), ".filescan-entries-*")
	if err != nil {
		return nil, fmt.Errorf("create entry sidecar: %w", err)
	}
	entriesTempName := entriesTemp.Name()
	defer os.Remove(entriesTempName)

	entriesHash := sha256.New()
	result, scanErr := scan(ctx, root, true, io.MultiWriter(entriesTemp, entriesHash))
	if scanErr != nil {
		_ = entriesTemp.Close()
		return nil, scanErr
	}
	if err := entriesTemp.Sync(); err != nil {
		_ = entriesTemp.Close()
		return nil, fmt.Errorf("sync entry sidecar: %w", err)
	}
	if err := entriesTemp.Close(); err != nil {
		return nil, fmt.Errorf("close entry sidecar: %w", err)
	}
	entriesDigest := hex.EncodeToString(entriesHash.Sum(nil))
	entriesPath := filepath.Join(filepath.Dir(output), "filescan-entries-"+entriesDigest+".bin")

	manifest := &Manifest{
		SchemaVersion:   ManifestSchemaVersion,
		ID:              options.ID,
		WorkloadClass:   options.WorkloadClass,
		Role:            options.Role,
		Root:            root,
		CreatedUTC:      time.Now().UTC(),
		Source:          options.Source,
		EntriesFile:     filepath.Base(entriesPath),
		EntriesSHA256:   entriesDigest,
		PortableDigest:  result.portableDigest,
		LiveGuardDigest: result.liveGuardDigest,
		Summary:         result.summary,
	}
	if err := manifest.Validate(); err != nil {
		return nil, fmt.Errorf("validate corpus manifest: %w", err)
	}
	if err := commitContentAddressed(entriesTempName, entriesPath, entriesDigest); err != nil {
		return nil, fmt.Errorf("commit entry sidecar: %w", err)
	}
	if err := writeManifest(output, manifest, options.Force); err != nil {
		return nil, err
	}
	return manifest, nil
}

func commitContentAddressed(tempPath, targetPath, expectedDigest string) error {
	if err := commitTemp(tempPath, targetPath, false); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}
	observed, err := hashFile(targetPath)
	if err != nil {
		return err
	}
	if observed != expectedDigest {
		return fmt.Errorf("content-addressed sidecar collision at %q", targetPath)
	}
	return nil
}

// Load strictly decodes a manifest and validates its version and digests.
func Load(path string) (*Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open corpus manifest: %w", err)
	}
	defer file.Close()
	var manifest Manifest
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode corpus manifest: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// Validate checks the manifest wire contract.
func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("manifest schema_version %d, want %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if m.ID == "" || m.WorkloadClass == "" || m.Root == "" {
		return errors.New("manifest id, workload_class, role, and root are required")
	}
	if !validRole(m.Role) {
		return fmt.Errorf("manifest role %q is invalid", m.Role)
	}
	if m.CreatedUTC.IsZero() || m.CreatedUTC.Location() != time.UTC {
		return errors.New("manifest created_utc must be a nonzero UTC timestamp")
	}
	if m.Source.Origin == "" || m.Source.Revision == "" {
		return errors.New("manifest source origin and revision are required")
	}
	if !filepath.IsAbs(m.Root) || filepath.Clean(m.Root) != m.Root {
		return errors.New("manifest root must be an absolute clean path")
	}
	if m.EntriesFile == "" || filepath.IsAbs(m.EntriesFile) || !filepath.IsLocal(m.EntriesFile) {
		return errors.New("manifest entries_file must be a local relative path")
	}
	for name, digest := range map[string]string{
		"entries_sha256":          m.EntriesSHA256,
		"portable_content_digest": m.PortableDigest,
		"live_guard_digest":       m.LiveGuardDigest,
	} {
		if err := validateSHA256(name, digest); err != nil {
			return err
		}
	}
	if err := protocol.ValidateExpected(m.ExpectedOutputs); err != nil {
		return fmt.Errorf("validate expected outputs: %w", err)
	}
	frozen := m.ExpectedOutputs.Targets.Digest != ""
	if frozen != (m.Oracle != nil) {
		return errors.New("manifest oracle metadata and frozen outputs must be present together")
	}
	if m.Oracle != nil && (m.Oracle.FrozenUTC.IsZero() || m.Oracle.FrozenUTC.Location() != time.UTC) {
		return errors.New("manifest oracle frozen_utc must be a nonzero UTC timestamp")
	}
	if m.Summary.MaxDepth < 0 || m.Summary.FileSizeBins == nil || m.Summary.FileExtensions == nil ||
		m.Summary.ArchiveExtensions == nil || m.Summary.ErrorCategories == nil {
		return errors.New("manifest summary maps are required and max_depth must be nonnegative")
	}
	return nil
}

func validRole(role string) bool {
	switch role {
	case "calibration", "confirmation", "correctness":
		return true
	default:
		return false
	}
}

// Verify checks one or more frozen corpus identities. Valid modes are live,
// portable, sidecar, and all.
func Verify(ctx context.Context, manifestPath, root, mode string) (VerifyResult, error) {
	manifest, err := Load(manifestPath)
	if err != nil {
		return VerifyResult{}, err
	}
	if root == "" {
		root = manifest.Root
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("resolve verify root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("resolve verify root symlinks: %w", err)
	}
	result := VerifyResult{
		SchemaVersion:       ManifestSchemaVersion,
		ManifestPath:        manifestPath,
		Root:                root,
		Mode:                mode,
		Valid:               true,
		ExpectedPortable:    manifest.PortableDigest,
		ExpectedLiveGuard:   manifest.LiveGuardDigest,
		ExpectedEntriesHash: manifest.EntriesSHA256,
	}

	switch mode {
	case "live":
		scanResult, err := scan(ctx, root, false, nil)
		if err != nil {
			return VerifyResult{}, err
		}
		result.ObservedLiveGuard = scanResult.liveGuardDigest
		result.ObservedSummary = &scanResult.summary
		compareDigest(&result, "live_guard_mismatch", manifest.LiveGuardDigest, result.ObservedLiveGuard)
	case "portable":
		scanResult, err := scan(ctx, root, true, nil)
		if err != nil {
			return VerifyResult{}, err
		}
		result.ObservedPortable = scanResult.portableDigest
		result.ObservedSummary = &scanResult.summary
		compareDigest(&result, "portable_content_mismatch", manifest.PortableDigest, result.ObservedPortable)
	case "sidecar":
		observed, err := hashFile(entriesPath(manifestPath, manifest.EntriesFile))
		if err != nil {
			return VerifyResult{}, fmt.Errorf("hash entry sidecar: %w", err)
		}
		result.ObservedEntriesHash = observed
		compareDigest(&result, "entry_sidecar_mismatch", manifest.EntriesSHA256, observed)
	case "all":
		scanResult, err := scan(ctx, root, true, nil)
		if err != nil {
			return VerifyResult{}, err
		}
		result.ObservedPortable = scanResult.portableDigest
		result.ObservedLiveGuard = scanResult.liveGuardDigest
		result.ObservedSummary = &scanResult.summary
		compareDigest(&result, "portable_content_mismatch", manifest.PortableDigest, result.ObservedPortable)
		compareDigest(&result, "live_guard_mismatch", manifest.LiveGuardDigest, result.ObservedLiveGuard)
		observed, err := hashFile(entriesPath(manifestPath, manifest.EntriesFile))
		if err != nil {
			return VerifyResult{}, fmt.Errorf("hash entry sidecar: %w", err)
		}
		result.ObservedEntriesHash = observed
		compareDigest(&result, "entry_sidecar_mismatch", manifest.EntriesSHA256, observed)
	default:
		return VerifyResult{}, fmt.Errorf("unknown verify mode %q", mode)
	}
	return result, nil
}

// SetOracle atomically freezes scanner output identities in an existing
// manifest.
func SetOracle(ctx context.Context, path string, metrics protocol.ScannerMetrics) (*Manifest, error) {
	if err := metrics.Validate(); err != nil {
		return nil, err
	}
	manifest, err := Load(path)
	if err != nil {
		return nil, err
	}
	verification, err := Verify(ctx, path, manifest.Root, "all")
	if err != nil {
		return nil, fmt.Errorf("verify corpus before freezing oracle: %w", err)
	}
	if !verification.Valid {
		return nil, errors.New("corpus verification failed before freezing oracle")
	}
	manifest.ExpectedOutputs = metrics.Outputs()
	manifest.Oracle = &Oracle{FrozenUTC: time.Now().UTC(), Candidate: metrics.Candidate}
	if err := writeManifest(path, manifest, true); err != nil {
		return nil, err
	}
	return manifest, nil
}

// FileSHA256 returns the SHA-256 digest of a file without loading it entirely
// into memory.
func FileSHA256(path string) (string, error) {
	return hashFile(path)
}

func writeManifest(path string, manifest *Manifest, force bool) error {
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("validate corpus manifest before write: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode corpus manifest: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data, force)
}

func writeFileAtomic(path string, data []byte, force bool) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".filescan-manifest-*")
	if err != nil {
		return fmt.Errorf("create temporary manifest: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary manifest: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary manifest: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary manifest: %w", err)
	}
	if err := commitTemp(tempName, path, force); err != nil {
		return fmt.Errorf("commit manifest: %w", err)
	}
	return nil
}

func commitTemp(tempPath, targetPath string, force bool) error {
	if force {
		if err := os.Rename(tempPath, targetPath); err != nil {
			return err
		}
	} else {
		if err := os.Link(tempPath, targetPath); err != nil {
			return err
		}
		if err := os.Remove(tempPath); err != nil {
			return err
		}
	}
	directory, err := os.Open(filepath.Dir(targetPath))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func entriesPath(manifestPath, entryFile string) string {
	return filepath.Join(filepath.Dir(manifestPath), entryFile)
}

func compareDigest(result *VerifyResult, code, expected, observed string) {
	if expected == observed {
		return
	}
	result.Valid = false
	result.Validity = append(result.Validity, protocol.LedgerEntry{
		Code: code, Severity: "error", Owner: "infrastructure",
		Message: fmt.Sprintf("observed %s; expected %s", observed, expected),
	})
}

func validateSHA256(name, digest string) error {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("%s must be 64 hexadecimal characters", name)
	}
	if digest != strings.ToLower(digest) {
		return fmt.Errorf("%s must use lowercase hexadecimal", name)
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func pathWithin(root, candidate string) (bool, error) {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, fmt.Errorf("compare corpus and output paths: %w", err)
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func resolveAllowMissing(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func requireEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("decode corpus manifest: trailing JSON value")
		}
		return fmt.Errorf("decode corpus manifest trailing data: %w", err)
	}
	return nil
}

// MarshalVerifyResult encodes a verification result as stable indented JSON.
func MarshalVerifyResult(result VerifyResult) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
