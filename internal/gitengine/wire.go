package gitengine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxFrame bounds every helper allocation to 16 MiB. Large hunks must
// be rejected explicitly or split by a future protocol version.
const DefaultMaxFrame uint32 = 16 << 20

// FrameTag is a four-byte wire frame discriminator.
type FrameTag [4]byte

var (
	TagHello      = FrameTag{'H', 'E', 'L', 'O'}
	TagBatchBegin = FrameTag{'B', 'G', 'I', 'N'}
	TagCommit     = FrameTag{'C', 'M', 'I', 'T'}
	TagFileBegin  = FrameTag{'F', 'B', 'E', 'G'}
	TagHunk       = FrameTag{'H', 'U', 'N', 'K'}
	TagHunkBegin  = FrameTag{'H', 'B', 'G', 'N'}
	TagHunkAdd    = FrameTag{'H', 'A', 'D', 'D'}
	TagHunkEnd    = FrameTag{'H', 'E', 'N', 'D'}
	TagFileEnd    = FrameTag{'F', 'E', 'N', 'D'}
	TagCommitEnd  = FrameTag{'C', 'E', 'N', 'D'}
	TagBatchEnd   = FrameTag{'B', 'E', 'N', 'D'}
	TagError      = FrameTag{'E', 'R', 'R', 'O'}
	TagQuit       = FrameTag{'Q', 'U', 'I', 'T'}
)

// Frame is the bounded transport unit. Payload always owns its bytes.
type Frame struct {
	Tag     FrameTag
	BatchID uint64
	Payload []byte
}

// WriteFrame writes one length-prefixed frame.
func WriteFrame(w io.Writer, frame Frame, max uint32) error {
	if max == 0 {
		max = DefaultMaxFrame
	}
	length := uint64(12) + uint64(len(frame.Payload))
	if length > uint64(max) {
		return fmt.Errorf("frame length %d exceeds limit %d", length, max)
	}
	var header [16]byte
	binary.BigEndian.PutUint32(header[:4], uint32(length))
	copy(header[4:8], frame.Tag[:])
	binary.BigEndian.PutUint64(header[8:16], frame.BatchID)
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	if len(frame.Payload) == 0 {
		return nil
	}
	return writeAll(w, frame.Payload)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// ReadFrame reads one frame and rejects truncated or oversized input before
// allocating the payload.
func ReadFrame(r io.Reader, max uint32) (Frame, error) {
	if max == 0 {
		max = DefaultMaxFrame
	}
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(prefix[:])
	if length < 12 {
		return Frame{}, fmt.Errorf("frame length %d is shorter than header", length)
	}
	if length > max {
		return Frame{}, fmt.Errorf("frame length %d exceeds limit %d", length, max)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return Frame{}, err
	}
	var tag FrameTag
	copy(tag[:], data[:4])
	return Frame{
		Tag:     tag,
		BatchID: binary.BigEndian.Uint64(data[4:12]),
		Payload: data[12:],
	}, nil
}

// HelloFrame encodes negotiated capabilities.
func HelloFrame(capabilities Capabilities) Frame {
	var payload []byte
	payload = binary.BigEndian.AppendUint16(payload, capabilities.ProtocolVersion)
	payload = binary.BigEndian.AppendUint16(payload, capabilities.OIDLength)
	payload = appendBool(payload, capabilities.AllStatuses)
	payload = appendBool(payload, capabilities.FindCopies)
	return Frame{Tag: TagHello, Payload: payload}
}

// DecodeHello decodes and validates a HELO frame.
func DecodeHello(frame Frame) (Capabilities, error) {
	if frame.Tag != TagHello || frame.BatchID != 0 || len(frame.Payload) != 6 {
		return Capabilities{}, errors.New("invalid HELO frame")
	}
	capabilities := Capabilities{
		ProtocolVersion: binary.BigEndian.Uint16(frame.Payload[:2]),
		OIDLength:       binary.BigEndian.Uint16(frame.Payload[2:4]),
		AllStatuses:     frame.Payload[4] != 0,
		FindCopies:      frame.Payload[5] != 0,
	}
	if frame.Payload[4] > 1 || frame.Payload[5] > 1 {
		return Capabilities{}, errors.New("invalid HELO boolean")
	}
	if err := capabilities.Validate(); err != nil {
		return Capabilities{}, err
	}
	return capabilities, nil
}

// BatchBeginFrame encodes a bounded request.
func BatchBeginFrame(request BatchRequest) (Frame, error) {
	if err := request.Validate(0); err != nil {
		return Frame{}, err
	}
	if uint64(len(request.Commits)) > uint64(^uint32(0)) {
		return Frame{}, errors.New("batch commit count exceeds protocol limit")
	}
	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, uint32(len(request.Commits)))
	for _, oid := range request.Commits {
		if err := validateOID(oid, false); err != nil {
			return Frame{}, err
		}
		payload = appendBytes(payload, oid)
	}
	return Frame{Tag: TagBatchBegin, BatchID: request.ID, Payload: payload}, nil
}

// DecodeBatchBegin decodes a BGIN request and owns every OID.
func DecodeBatchBegin(frame Frame) (BatchRequest, error) {
	if frame.Tag != TagBatchBegin || frame.BatchID == 0 {
		return BatchRequest{}, errors.New("invalid BGIN frame")
	}
	d := decoder{data: frame.Payload}
	count, err := d.u32()
	if err != nil {
		return BatchRequest{}, err
	}
	if uint64(count)*24 > uint64(len(d.data)-d.pos) {
		return BatchRequest{}, errors.New("batch commit count exceeds bounded payload")
	}
	commits := make([]OID, 0, count)
	for range count {
		oid, err := d.bytes()
		if err != nil {
			return BatchRequest{}, err
		}
		if err := validateOID(oid, false); err != nil {
			return BatchRequest{}, err
		}
		commits = append(commits, OID(oid))
	}
	if err := d.done(); err != nil {
		return BatchRequest{}, err
	}
	request := BatchRequest{ID: frame.BatchID, Commits: commits}
	if err := request.Validate(0); err != nil {
		return BatchRequest{}, err
	}
	return request, nil
}

// RecordFrame encodes a typed record using its lifecycle frame tag.
func RecordFrame(batchID uint64, record Record) (Frame, error) {
	if batchID == 0 {
		return Frame{}, errors.New("record batch ID is zero")
	}
	payload, err := encodeRecordPayload(record)
	if err != nil {
		return Frame{}, err
	}
	var tag FrameTag
	switch record.Kind {
	case RecordCommit:
		tag = TagCommit
	case RecordFile:
		tag = TagFileBegin
	case RecordHunk:
		tag = TagHunk
	}
	return Frame{Tag: tag, BatchID: batchID, Payload: payload}, nil
}

// DecodeRecord decodes CMIT, FBEG, or HUNK and owns every byte sequence.
func DecodeRecord(frame Frame) (Record, error) {
	if frame.BatchID == 0 {
		return Record{}, errors.New("record frame batch ID is zero")
	}
	var kind RecordKind
	switch frame.Tag {
	case TagCommit:
		kind = RecordCommit
	case TagFileBegin:
		kind = RecordFile
	case TagHunk:
		kind = RecordHunk
	default:
		return Record{}, fmt.Errorf("frame %.4s is not a record", frame.Tag)
	}
	record, err := decodeRecordPayload(kind, frame.Payload)
	if err != nil {
		return Record{}, err
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// HunkFrames returns one HUNK frame when it fits, otherwise a bounded
// HBGN/HADD+/HEND continuation sequence. The assembled record remains subject
// to the receiver's independent MaxRecord bound.
func HunkFrames(batchID uint64, hunk HunkRecord, max uint32) ([]Frame, error) {
	if batchID == 0 {
		return nil, errors.New("hunk batch ID is zero")
	}
	if max == 0 {
		max = DefaultMaxFrame
	}
	if max <= 12 {
		return nil, errors.New("frame limit cannot hold payload")
	}
	record := Record{Kind: RecordHunk, Hunk: hunk}
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	regularSize := uint64(12 + 4 + len(hunk.Commit) + 4 + len(hunk.NewPath) + 8 + 4 + len(hunk.Added) + 1)
	if regularSize <= uint64(max) {
		frame, err := RecordFrame(batchID, record)
		if err != nil {
			return nil, err
		}
		return []Frame{frame}, nil
	}
	beginPayload := encodeHunkBegin(hunk)
	if uint64(12+len(beginPayload)) > uint64(max) {
		return nil, errors.New("HBGN metadata exceeds frame limit")
	}
	if len(hunk.Added) == 0 {
		return nil, errors.New("continued hunk requires nonempty added bytes")
	}
	frames := []Frame{{Tag: TagHunkBegin, BatchID: batchID, Payload: beginPayload}}
	chunkSize := int(max) - 12
	for offset := 0; offset < len(hunk.Added); offset += chunkSize {
		end := min(offset+chunkSize, len(hunk.Added))
		frames = append(frames, Frame{Tag: TagHunkAdd, BatchID: batchID, Payload: bytes.Clone(hunk.Added[offset:end])})
	}
	frames = append(frames, Frame{Tag: TagHunkEnd, BatchID: batchID, Payload: appendBool(nil, hunk.MissingFinalNewline)})
	return frames, nil
}

// DecodeHunkBegin decodes continuation metadata without added bytes.
func DecodeHunkBegin(frame Frame) (HunkRecord, error) {
	if frame.Tag != TagHunkBegin || frame.BatchID == 0 {
		return HunkRecord{}, errors.New("invalid HBGN frame")
	}
	d := decoder{data: frame.Payload}
	commit, err := d.oid(false)
	if err != nil {
		return HunkRecord{}, err
	}
	path, err := d.bytes()
	if err != nil {
		return HunkRecord{}, err
	}
	position, err := d.u64()
	if err != nil {
		return HunkRecord{}, err
	}
	if err := d.done(); err != nil {
		return HunkRecord{}, err
	}
	hunk := HunkRecord{Commit: commit, NewPath: path, NewPosition: position}
	if err := validateRecord(Record{Kind: RecordHunk, Hunk: hunk}); err != nil {
		return HunkRecord{}, err
	}
	return hunk, nil
}

// DecodeHunkEnd decodes the final-newline state discovered after streamed
// added bytes have already been emitted.
func DecodeHunkEnd(frame Frame) (bool, error) {
	if frame.Tag != TagHunkEnd || frame.BatchID == 0 || len(frame.Payload) != 1 {
		return false, errors.New("invalid HEND frame")
	}
	if frame.Payload[0] > 1 {
		return false, errors.New("invalid HEND boolean")
	}
	return frame.Payload[0] == 1, nil
}

func encodeHunkBegin(hunk HunkRecord) []byte {
	var payload []byte
	payload = appendBytes(payload, hunk.Commit)
	payload = appendBytes(payload, hunk.NewPath)
	payload = binary.BigEndian.AppendUint64(payload, hunk.NewPosition)
	return payload
}

// BatchEndFrame encodes the helper-confirmed result.
func BatchEndFrame(result BatchResult) Frame {
	payload := []byte{byte(result.Status)}
	payload = binary.BigEndian.AppendUint64(payload, result.Commits)
	payload = binary.BigEndian.AppendUint64(payload, result.Files)
	payload = binary.BigEndian.AppendUint64(payload, result.Hunks)
	return Frame{Tag: TagBatchEnd, BatchID: result.ID, Payload: payload}
}

// DecodeBatchEnd decodes a BEND frame.
func DecodeBatchEnd(frame Frame) (BatchResult, error) {
	if frame.Tag != TagBatchEnd || frame.BatchID == 0 || len(frame.Payload) != 25 {
		return BatchResult{}, errors.New("invalid BEND frame")
	}
	status := CompletionStatus(frame.Payload[0])
	if status != CompletionOK && status != CompletionRejected {
		return BatchResult{}, errors.New("invalid BEND status")
	}
	return BatchResult{
		ID:      frame.BatchID,
		Status:  status,
		Commits: binary.BigEndian.Uint64(frame.Payload[1:9]),
		Files:   binary.BigEndian.Uint64(frame.Payload[9:17]),
		Hunks:   binary.BigEndian.Uint64(frame.Payload[17:25]),
	}, nil
}

// ErrorFrame encodes a typed terminal helper error.
func ErrorFrame(batchID uint64, kind ErrorKind, op string, err error) Frame {
	payload := []byte{byte(kind)}
	payload = appendBytes(payload, []byte(op))
	payload = appendBytes(payload, []byte(err.Error()))
	return Frame{Tag: TagError, BatchID: batchID, Payload: payload}
}

// DecodeError decodes an ERRO frame.
func DecodeError(frame Frame) (*EngineError, error) {
	if frame.Tag != TagError || len(frame.Payload) < 1 {
		return nil, errors.New("invalid ERRO frame")
	}
	kind := ErrorKind(frame.Payload[0])
	if kind < ErrorProtocol || kind > ErrorUnsupported {
		return nil, errors.New("invalid ERRO kind")
	}
	d := decoder{data: frame.Payload[1:]}
	op, err := d.bytes()
	if err != nil {
		return nil, err
	}
	message, err := d.bytes()
	if err != nil {
		return nil, err
	}
	if err := d.done(); err != nil {
		return nil, err
	}
	return &EngineError{Kind: kind, BatchID: frame.BatchID, Op: string(op), Err: errors.New(string(message))}, nil
}

func encodeRecordPayload(record Record) ([]byte, error) {
	canonical, err := CanonicalRecord(record)
	if err != nil {
		return nil, err
	}
	return canonical[1:], nil
}

func decodeRecordPayload(kind RecordKind, payload []byte) (Record, error) {
	d := decoder{data: payload}
	record := Record{Kind: kind}
	var err error
	switch kind {
	case RecordCommit:
		r := &record.Commit
		if r.OID, err = d.oid(false); err != nil {
			return Record{}, err
		}
		if r.Message, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.AuthorName, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.AuthorEmail, err = d.bytes(); err != nil {
			return Record{}, err
		}
		v, e := d.u64()
		if e != nil {
			return Record{}, e
		}
		r.AuthorUnixSeconds = int64(v)
		u, e := d.u32()
		if e != nil {
			return Record{}, e
		}
		r.AuthorUTCOffsetMinutes = int32(u)
		if r.HasAuthor, err = d.boolean(); err != nil {
			return Record{}, err
		}
		if r.HasAuthorTime, err = d.boolean(); err != nil {
			return Record{}, err
		}
	case RecordFile:
		r := &record.File
		if r.Commit, err = d.oid(false); err != nil {
			return Record{}, err
		}
		status, e := d.byte()
		if e != nil {
			return Record{}, e
		}
		r.Status = Status(status)
		if r.OldMode, err = d.u32(); err != nil {
			return Record{}, err
		}
		if r.NewMode, err = d.u32(); err != nil {
			return Record{}, err
		}
		if r.OldOID, err = d.oid(true); err != nil {
			return Record{}, err
		}
		if r.NewOID, err = d.oid(true); err != nil {
			return Record{}, err
		}
		if r.OldPath, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.NewPath, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.Binary, err = d.boolean(); err != nil {
			return Record{}, err
		}
	case RecordHunk:
		r := &record.Hunk
		if r.Commit, err = d.oid(false); err != nil {
			return Record{}, err
		}
		if r.NewPath, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.NewPosition, err = d.u64(); err != nil {
			return Record{}, err
		}
		if r.Added, err = d.bytes(); err != nil {
			return Record{}, err
		}
		if r.MissingFinalNewline, err = d.boolean(); err != nil {
			return Record{}, err
		}
	}
	if err := d.done(); err != nil {
		return Record{}, err
	}
	return record, nil
}

func validateOID(oid []byte, allowEmpty bool) error {
	if allowEmpty && len(oid) == 0 {
		return nil
	}
	if len(oid) != 20 && len(oid) != 32 {
		return fmt.Errorf("object ID length %d", len(oid))
	}
	return nil
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) byte() (byte, error) {
	if d.pos >= len(d.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := d.data[d.pos]
	d.pos++
	return v, nil
}

func (d *decoder) boolean() (bool, error) {
	v, err := d.byte()
	if err != nil {
		return false, err
	}
	if v > 1 {
		return false, errors.New("invalid boolean")
	}
	return v == 1, nil
}

func (d *decoder) u32() (uint32, error) {
	if len(d.data)-d.pos < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.BigEndian.Uint32(d.data[d.pos : d.pos+4])
	d.pos += 4
	return v, nil
}

func (d *decoder) u64() (uint64, error) {
	if len(d.data)-d.pos < 8 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.BigEndian.Uint64(d.data[d.pos : d.pos+8])
	d.pos += 8
	return v, nil
}

func (d *decoder) bytes() ([]byte, error) {
	n, err := d.u32()
	if err != nil {
		return nil, err
	}
	if uint64(n) > uint64(len(d.data)-d.pos) {
		return nil, io.ErrUnexpectedEOF
	}
	v := append([]byte(nil), d.data[d.pos:d.pos+int(n)]...)
	d.pos += int(n)
	return v, nil
}

func (d *decoder) oid(allowEmpty bool) (OID, error) {
	v, err := d.bytes()
	if err != nil {
		return nil, err
	}
	if err := validateOID(v, allowEmpty); err != nil {
		return nil, err
	}
	return OID(v), nil
}

func (d *decoder) done() error {
	if d.pos != len(d.data) {
		return fmt.Errorf("%d trailing payload bytes", len(d.data)-d.pos)
	}
	return nil
}
