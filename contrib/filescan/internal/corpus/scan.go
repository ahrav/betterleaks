package corpus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const entriesHeader = "betterleaks-filescan-entries-v1\x00"

type scanResult struct {
	portableDigest  string
	liveGuardDigest string
	summary         Summary
}

type entryRecord struct {
	Kind          byte
	Path          string
	Mode          uint32
	Size          int64
	PhysicalBytes uint64
	LinkTarget    string
	ContentDigest [sha256.Size]byte
	ContentError  string
	Metadata      metadata
}

type metadata struct {
	Device  uint64
	Inode   uint64
	Links   uint64
	UID     uint64
	GID     uint64
	Blocks  uint64
	MTimeNS int64
	CTimeNS int64
}

func scan(ctx context.Context, root string, hashContent bool, sidecar io.Writer) (scanResult, error) {
	portableHash := sha256.New()
	liveHash := sha256.New()
	_, _ = portableHash.Write([]byte("betterleaks-filescan-portable-v1\x00"))
	_, _ = liveHash.Write([]byte("betterleaks-filescan-live-v1\x00"))
	if sidecar != nil {
		if _, err := io.WriteString(sidecar, entriesHeader); err != nil {
			return scanResult{}, fmt.Errorf("write entry sidecar header: %w", err)
		}
	}

	summary := Summary{
		FileSizeBins:      make(map[string]uint64),
		FileExtensions:    make(map[string]uint64),
		ArchiveExtensions: make(map[string]uint64),
		ErrorCategories:   make(map[string]uint64),
	}
	fanout := make(map[string]uint64)
	readBuffer := make([]byte, 1024*1024)

	process := func(record entryRecord) error {
		if err := addDigestRecord(portableHash, portableRecord(record)); err != nil {
			return err
		}
		if err := addDigestRecord(liveHash, liveRecord(record)); err != nil {
			return err
		}
		if sidecar != nil {
			if err := writeLengthPrefixed(sidecar, sidecarRecord(record)); err != nil {
				return fmt.Errorf("write entry sidecar record: %w", err)
			}
		}
		updateSummary(&summary, fanout, record)
		return nil
	}

	err := filepath.WalkDir(root, func(path string, dirEntry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("make corpus path relative: %w", err)
		}
		relative = filepath.ToSlash(relative)
		if walkErr != nil {
			category := errorCategory(walkErr)
			if err := process(entryRecord{Kind: 'e', Path: relative, ContentError: category}); err != nil {
				return err
			}
			if dirEntry != nil && dirEntry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		info, err := os.Lstat(path)
		if err != nil {
			return process(entryRecord{Kind: 'e', Path: relative, ContentError: errorCategory(err)})
		}
		meta := metadataFor(info)
		record := entryRecord{
			Kind:          entryKind(info.Mode()),
			Path:          relative,
			Mode:          uint32(info.Mode()),
			Size:          info.Size(),
			PhysicalBytes: meta.Blocks * 512,
			Metadata:      meta,
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			record.LinkTarget, err = os.Readlink(path)
			if err != nil {
				record.ContentError = errorCategory(err)
			}
		}
		if info.Mode().IsRegular() && hashContent {
			digest, err := hashRegularFile(ctx, path, readBuffer)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				record.ContentError = errorCategory(err)
			} else {
				record.ContentDigest = digest
			}
		} else if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				record.ContentError = errorCategory(err)
			} else if err := file.Close(); err != nil {
				record.ContentError = errorCategory(err)
			}
		}
		return process(record)
	})
	if err != nil {
		return scanResult{}, fmt.Errorf("walk corpus: %w", err)
	}
	for _, count := range fanout {
		if count > summary.MaxFanout {
			summary.MaxFanout = count
		}
	}
	return scanResult{
		portableDigest:  hex.EncodeToString(portableHash.Sum(nil)),
		liveGuardDigest: hex.EncodeToString(liveHash.Sum(nil)),
		summary:         summary,
	}, nil
}

func hashRegularFile(ctx context.Context, path string, buffer []byte) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	hasher := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			if _, err := hasher.Write(buffer[:read]); err != nil {
				return zero, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return zero, readErr
		}
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}

func addDigestRecord(hasher hash.Hash, record []byte) error {
	return writeLengthPrefixed(hasher, record)
}

func writeLengthPrefixed(writer io.Writer, data []byte) error {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(data)))
	if _, err := writer.Write(length[:n]); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func portableRecord(record entryRecord) []byte {
	var buffer bytes.Buffer
	buffer.WriteByte(record.Kind)
	writeString(&buffer, record.Path)
	writeUint32(&buffer, record.Mode)
	writeInt64(&buffer, record.Size)
	writeString(&buffer, record.LinkTarget)
	buffer.Write(record.ContentDigest[:])
	writeString(&buffer, record.ContentError)
	return buffer.Bytes()
}

func liveRecord(record entryRecord) []byte {
	var buffer bytes.Buffer
	buffer.WriteByte(record.Kind)
	writeString(&buffer, record.Path)
	writeUint32(&buffer, record.Mode)
	writeInt64(&buffer, record.Size)
	writeUint64(&buffer, record.PhysicalBytes)
	writeString(&buffer, record.LinkTarget)
	writeString(&buffer, record.ContentError)
	writeUint64(&buffer, record.Metadata.Device)
	writeUint64(&buffer, record.Metadata.Inode)
	writeUint64(&buffer, record.Metadata.Links)
	writeUint64(&buffer, record.Metadata.UID)
	writeUint64(&buffer, record.Metadata.GID)
	writeUint64(&buffer, record.Metadata.Blocks)
	writeInt64(&buffer, record.Metadata.MTimeNS)
	writeInt64(&buffer, record.Metadata.CTimeNS)
	return buffer.Bytes()
}

func sidecarRecord(record entryRecord) []byte {
	var buffer bytes.Buffer
	portable := portableRecord(record)
	live := liveRecord(record)
	writeBytes(&buffer, portable)
	writeBytes(&buffer, live)
	return buffer.Bytes()
}

func writeString(buffer *bytes.Buffer, value string) {
	writeBytes(buffer, []byte(value))
}

func writeBytes(buffer *bytes.Buffer, value []byte) {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(len(value)))
	buffer.Write(length[:n])
	buffer.Write(value)
}

func writeUint32(buffer *bytes.Buffer, value uint32) {
	var data [4]byte
	binary.BigEndian.PutUint32(data[:], value)
	buffer.Write(data[:])
}

func writeUint64(buffer *bytes.Buffer, value uint64) {
	var data [8]byte
	binary.BigEndian.PutUint64(data[:], value)
	buffer.Write(data[:])
}

func writeInt64(buffer *bytes.Buffer, value int64) {
	writeUint64(buffer, uint64(value))
}

func entryKind(mode fs.FileMode) byte {
	switch {
	case mode.IsRegular():
		return 'f'
	case mode.IsDir():
		return 'd'
	case mode&fs.ModeSymlink != 0:
		return 'l'
	default:
		return 'o'
	}
}

func updateSummary(summary *Summary, fanout map[string]uint64, record entryRecord) {
	summary.Entries++
	if record.Path != "." {
		parent := filepath.ToSlash(filepath.Dir(record.Path))
		fanout[parent]++
	}
	depth := 0
	if record.Path != "." {
		depth = strings.Count(record.Path, "/") + 1
	}
	if depth > summary.MaxDepth {
		summary.MaxDepth = depth
	}
	if record.ContentError != "" {
		summary.ManifestErrors++
		summary.ErrorCategories[record.ContentError]++
	}
	switch record.Kind {
	case 'f':
		summary.RegularFiles++
		if record.Size > 0 {
			summary.LogicalBytes += uint64(record.Size)
		}
		summary.PhysicalBytes += record.PhysicalBytes
		if record.Size > 0 && record.PhysicalBytes < uint64(record.Size) {
			summary.SparseFiles++
		}
		summary.FileSizeBins[sizeBin(record.Size)]++
		extension := strings.ToLower(filepath.Ext(record.Path))
		if extension == "" {
			extension = "<none>"
		}
		summary.FileExtensions[extension]++
		if archive := archiveExtension(record.Path); archive != "" {
			summary.ArchiveExtensions[archive]++
		}
	case 'd':
		summary.Directories++
	case 'l':
		summary.Symlinks++
	case 'e':
		// ManifestErrors was incremented above.
	default:
		summary.OtherEntries++
	}
}

func sizeBin(size int64) string {
	switch {
	case size <= 1024:
		return "le_1k"
	case size <= 4*1024:
		return "1k_4k"
	case size <= 32*1024:
		return "4k_32k"
	case size <= 128*1024:
		return "32k_128k"
	case size <= 1024*1024:
		return "128k_1m"
	case size <= 16*1024*1024:
		return "1m_16m"
	default:
		return "gt_16m"
	}
}

func archiveExtension(path string) string {
	lower := strings.ToLower(path)
	for _, extension := range []string{
		".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tgz", ".tbz2", ".txz",
		".zip", ".7z", ".rar", ".tar", ".gz", ".bz2", ".xz", ".zst",
		".jar", ".war", ".whl", ".deb", ".rpm",
	} {
		if strings.HasSuffix(lower, extension) {
			return extension
		}
	}
	return ""
}

func errorCategory(err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return "permission"
	case errors.Is(err, fs.ErrNotExist):
		return "not_exist"
	case errors.Is(err, fs.ErrInvalid):
		return "invalid"
	default:
		return "io"
	}
}
