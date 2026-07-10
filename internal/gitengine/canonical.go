package gitengine

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

// CanonicalRecord encodes every field with explicit boundaries. The result is
// stable and safe for arbitrary path, identity, message, and hunk bytes.
func CanonicalRecord(record Record) ([]byte, error) {
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	var dst []byte
	dst = append(dst, byte(record.Kind))
	switch record.Kind {
	case RecordCommit:
		r := record.Commit
		dst = appendBytes(dst, r.OID)
		dst = appendBytes(dst, r.Message)
		dst = appendBytes(dst, r.AuthorName)
		dst = appendBytes(dst, r.AuthorEmail)
		dst = binary.BigEndian.AppendUint64(dst, uint64(r.AuthorUnixSeconds))
		dst = binary.BigEndian.AppendUint32(dst, uint32(r.AuthorUTCOffsetMinutes))
		dst = appendBool(dst, r.HasAuthor)
		dst = appendBool(dst, r.HasAuthorTime)
	case RecordFile:
		r := record.File
		dst = appendBytes(dst, r.Commit)
		dst = append(dst, byte(r.Status))
		dst = binary.BigEndian.AppendUint32(dst, r.OldMode)
		dst = binary.BigEndian.AppendUint32(dst, r.NewMode)
		dst = appendBytes(dst, r.OldOID)
		dst = appendBytes(dst, r.NewOID)
		dst = appendBytes(dst, r.OldPath)
		dst = appendBytes(dst, r.NewPath)
		dst = appendBool(dst, r.Binary)
	case RecordHunk:
		r := record.Hunk
		dst = appendBytes(dst, r.Commit)
		dst = appendBytes(dst, r.NewPath)
		dst = binary.BigEndian.AppendUint64(dst, r.NewPosition)
		dst = appendBytes(dst, r.Added)
		dst = appendBool(dst, r.MissingFinalNewline)
	}
	return dst, nil
}

// CanonicalMultiset encodes and sorts records while retaining duplicates.
func CanonicalMultiset(records []Record) ([][]byte, error) {
	bag := make([][]byte, len(records))
	for i, record := range records {
		encoded, err := CanonicalRecord(record)
		if err != nil {
			return nil, err
		}
		bag[i] = encoded
	}
	sort.Slice(bag, func(i, j int) bool { return bytes.Compare(bag[i], bag[j]) < 0 })
	return bag, nil
}

// CompareMultisets returns nil only when complete record multiplicity matches.
func CompareMultisets(left, right []Record) error {
	a, err := CanonicalMultiset(left)
	if err != nil {
		return err
	}
	b, err := CanonicalMultiset(right)
	if err != nil {
		return err
	}
	if len(a) != len(b) {
		return fmt.Errorf("record count %d != %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return fmt.Errorf("canonical record %d differs", i)
		}
	}
	return nil
}

func appendBytes(dst, value []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}

func appendBool(dst []byte, value bool) []byte {
	if value {
		return append(dst, 1)
	}
	return append(dst, 0)
}
