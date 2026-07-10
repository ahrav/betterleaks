// Package gitengine defines the engine-neutral boundary used to compare Git
// history implementations without coupling them to a patch-text parser.
package gitengine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
)

// ProtocolVersion is the only wire protocol version understood by this package.
const ProtocolVersion uint16 = 1

// Status is a Git raw-diff status byte.
type Status byte

const (
	StatusAdded      Status = 'A'
	StatusCopied     Status = 'C'
	StatusDeleted    Status = 'D'
	StatusModified   Status = 'M'
	StatusRenamed    Status = 'R'
	StatusTypeChange Status = 'T'
	StatusUnmerged   Status = 'U'
	StatusUnknown    Status = 'X'
	StatusBrokenPair Status = 'B'
)

// ProductionStatus reports whether status is part of the v1 scanner profile.
func ProductionStatus(status Status) bool {
	return status == StatusAdded || status == StatusModified || status == StatusRenamed
}

func validStatus(status Status) bool {
	switch status {
	case StatusAdded, StatusCopied, StatusDeleted, StatusModified, StatusRenamed,
		StatusTypeChange, StatusUnmerged, StatusUnknown, StatusBrokenPair:
		return true
	default:
		return false
	}
}

func validateRecord(record Record) error {
	switch record.Kind {
	case RecordCommit:
		return validateOID(record.Commit.OID, false)
	case RecordFile:
		if err := validateOID(record.File.Commit, false); err != nil {
			return err
		}
		if !validStatus(record.File.Status) {
			return fmt.Errorf("file status %q", record.File.Status)
		}
		if err := validateOID(record.File.OldOID, true); err != nil {
			return err
		}
		if err := validateOID(record.File.NewOID, true); err != nil {
			return err
		}
		if bytes.IndexByte(record.File.OldPath, 0) >= 0 || bytes.IndexByte(record.File.NewPath, 0) >= 0 {
			return errors.New("Git path contains NUL")
		}
		return nil
	case RecordHunk:
		if err := validateOID(record.Hunk.Commit, false); err != nil {
			return err
		}
		if bytes.IndexByte(record.Hunk.NewPath, 0) >= 0 {
			return errors.New("Git path contains NUL")
		}
		return nil
	default:
		return fmt.Errorf("record kind %d", record.Kind)
	}
}

// OID is an object identifier in raw binary form. Version 1 accepts SHA-1 and
// SHA-256 object IDs; Capabilities pins one width for a worker.
type OID []byte

// CommitRecord is the scanner-consumed commit metadata projection.
type CommitRecord struct {
	OID                    OID
	Message                []byte
	AuthorName             []byte
	AuthorEmail            []byte
	AuthorUnixSeconds      int64
	AuthorUTCOffsetMinutes int32
	HasAuthor              bool
	HasAuthorTime          bool
}

// FileRecord describes one post-diffcore file pair.
type FileRecord struct {
	Commit           OID
	Status           Status
	OldMode, NewMode uint32
	OldOID, NewOID   OID
	OldPath, NewPath []byte
	Binary           bool
}

// HunkRecord contains the exact scanner-visible added bytes for one hunk.
type HunkRecord struct {
	Commit              OID
	NewPath             []byte
	NewPosition         uint64
	Added               []byte
	MissingFinalNewline bool
}

// RecordKind identifies the active Record variant.
type RecordKind byte

const (
	RecordCommit RecordKind = iota + 1
	RecordFile
	RecordHunk
)

// Record is an engine-neutral tagged union. Exactly the field selected by
// Kind is meaningful.
type Record struct {
	Kind   RecordKind
	Commit CommitRecord
	File   FileRecord
	Hunk   HunkRecord
}

// ScanProfile identifies behavior that must be pinned across engines.
type ScanProfile struct {
	AllStatuses bool
	FindCopies  bool
}

// AllowsStatus reports whether a negotiated profile admits status.
func (p ScanProfile) AllowsStatus(status Status) bool {
	if !validStatus(status) {
		return false
	}
	if p.AllStatuses {
		return true
	}
	return ProductionStatus(status) || (p.FindCopies && status == StatusCopied)
}

// ProductionProfile returns the v1 A/M/R profile.
func ProductionProfile() ScanProfile { return ScanProfile{} }

// Capabilities are negotiated before workers emit records.
type Capabilities struct {
	ProtocolVersion uint16
	OIDLength       uint16
	AllStatuses     bool
	FindCopies      bool
}

// Validate rejects capabilities that cannot represent the version 1 contract.
func (c Capabilities) Validate() error {
	if c.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("protocol version %d: %w", c.ProtocolVersion, ErrUnsupported)
	}
	if c.OIDLength != 20 && c.OIDLength != 32 {
		return fmt.Errorf("object ID length %d: %w", c.OIDLength, ErrUnsupported)
	}
	return nil
}

// BatchRequest is one sequential request to a worker.
type BatchRequest struct {
	ID      uint64
	Commits []OID
}

// Validate checks request identity, negotiated OID width, and set semantics.
// Duplicate OIDs are rejected rather than silently deduplicated by Git's
// revision machinery. An empty commit set is valid.
func (r BatchRequest) Validate(oidLength int) error {
	if r.ID == 0 {
		return errors.New("git engine batch ID is zero")
	}
	seen := make(map[string]struct{}, len(r.Commits))
	for _, oid := range r.Commits {
		if oidLength == 0 {
			if err := validateOID(oid, false); err != nil {
				return err
			}
		} else if len(oid) != oidLength {
			return fmt.Errorf("batch object ID length %d != negotiated %d", len(oid), oidLength)
		}
		key := string(oid)
		if _, ok := seen[key]; ok {
			return errors.New("git engine batch contains duplicate object ID")
		}
		seen[key] = struct{}{}
	}
	return nil
}

// CompletionStatus is the terminal status carried by a successful BEND frame.
type CompletionStatus byte

const (
	CompletionOK CompletionStatus = iota
	CompletionRejected
)

// BatchResult reports counters confirmed by the helper's terminal frame.
type BatchResult struct {
	ID      uint64
	Status  CompletionStatus
	Commits uint64
	Files   uint64
	Hunks   uint64
}

// Factory is the consumer-side seam shared by replacement engines.
type Factory interface {
	Preflight(context.Context, string, ScanProfile) (Capabilities, error)
	Open(context.Context, string, ScanProfile) (Worker, error)
}

// Worker processes one batch at a time. Callers create multiple workers for
// concurrency rather than calling ScanBatch concurrently on one worker.
type Worker interface {
	ScanBatch(context.Context, BatchRequest, EmitFunc) (BatchResult, error)
	Close() error
}

// EmitFunc consumes one owned record. EmitFunc must return promptly, honor
// cancellation through the ScanBatch context, and must not call methods on the
// same Worker reentrantly. Go cannot forcibly stop an arbitrary callback;
// cooperative return is required for ScanBatch cancellation to finish without
// leaking its protocol reader.
type EmitFunc func(Record) error

// ErrUnsupported is returned only during repository-wide preflight, before
// any worker has emitted a record, so the caller may choose a fallback.
var ErrUnsupported = errors.New("git engine unsupported")

// ErrorKind classifies failures without requiring callers to parse text.
type ErrorKind byte

const (
	ErrorProtocol ErrorKind = iota + 1
	ErrorHelper
	ErrorCanceled
	ErrorEmission
	// ErrorUnsupported is valid only in a batch-zero ERRO frame before HELO.
	// It is the helper-side signal that repository-wide preflight may safely
	// choose another engine before any worker emits a record.
	ErrorUnsupported
)

// EngineError is a typed terminal worker failure. Runtime errors are never
// safe signals for falling back after workers have started.
type EngineError struct {
	Kind    ErrorKind
	BatchID uint64
	Op      string
	Stderr  string
	Err     error
}

func (e *EngineError) Error() string {
	if e.BatchID != 0 {
		return fmt.Sprintf("git engine %s batch %d: %v", e.Op, e.BatchID, e.Err)
	}
	return fmt.Sprintf("git engine %s: %v", e.Op, e.Err)
}

// Unwrap returns the underlying failure.
func (e *EngineError) Unwrap() error { return e.Err }
