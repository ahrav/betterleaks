package gitengine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func testOID(n int, value byte) OID { return bytes.Repeat([]byte{value}, n) }

func testRecords(oidLength int) []Record {
	oid := testOID(oidLength, 0x11)
	return []Record{
		{Kind: RecordCommit, Commit: CommitRecord{
			OID: oid, Message: []byte("message\x00\xff"), AuthorName: []byte("name\n"),
			AuthorEmail: []byte("a@example.test"), AuthorUnixSeconds: -123,
			AuthorUTCOffsetMinutes: -420, HasAuthor: true, HasAuthorTime: true,
		}},
		{Kind: RecordFile, File: FileRecord{
			Commit: oid, Status: StatusRenamed, OldMode: 0o100644, NewMode: 0o100755,
			OldOID: testOID(oidLength, 0x22), NewOID: testOID(oidLength, 0x33),
			OldPath: []byte("old\n\xff"), NewPath: []byte("new\tboundary"), Binary: true,
		}},
		{Kind: RecordHunk, Hunk: HunkRecord{
			Commit: oid, NewPath: []byte("new\tboundary"), NewPosition: 42,
			Added: []byte("\x00line\r\n\xff"), MissingFinalNewline: true,
		}},
	}
}

func TestRecordCodecRoundTripOwnsBytes(t *testing.T) {
	for _, oidLength := range []int{20, 32} {
		for _, want := range testRecords(oidLength) {
			frame, err := RecordFrame(9, want)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			if err := WriteFrame(&wire, frame, 0); err != nil {
				t.Fatal(err)
			}
			decodedFrame, err := ReadFrame(&wire, 0)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeRecord(decodedFrame)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := CanonicalRecord(got)
			clear(decodedFrame.Payload)
			after, _ := CanonicalRecord(got)
			if !bytes.Equal(before, after) {
				t.Fatal("decoded record aliases frame payload")
			}
			wantCanonical, _ := CanonicalRecord(want)
			if !bytes.Equal(after, wantCanonical) {
				t.Fatalf("round trip differs for kind %d", want.Kind)
			}
		}
	}
}

func TestFrameBoundsAndTruncation(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
		max  uint32
		err  error
	}{
		{name: "empty", err: io.EOF},
		{name: "short prefix", wire: []byte{0, 0}, err: io.ErrUnexpectedEOF},
		{name: "short header", wire: []byte{0, 0, 0, 11}, err: nil},
		{name: "oversize", wire: []byte{0, 0, 1, 0}, max: 32, err: nil},
		{name: "mid frame", wire: append([]byte{0, 0, 0, 12}, make([]byte, 8)...), err: io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tt.wire), tt.max)
			if err == nil {
				t.Fatal("ReadFrame succeeded")
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("ReadFrame error = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestBatchCodecCommitOnlyAndSHAWidths(t *testing.T) {
	for _, length := range []int{20, 32} {
		request := BatchRequest{ID: 7, Commits: []OID{testOID(length, 1), testOID(length, 2)}}
		frame, err := BatchBeginFrame(request)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeBatchBegin(frame)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != request.ID || len(got.Commits) != len(request.Commits) {
			t.Fatalf("DecodeBatchBegin = %#v", got)
		}
		clear(frame.Payload)
		if !bytes.Equal(got.Commits[0], testOID(length, 1)) {
			t.Fatal("decoded batch aliases frame payload")
		}
	}
}

func TestBatchDecoderRejectsCountBeforeAllocation(t *testing.T) {
	frame := Frame{Tag: TagBatchBegin, BatchID: 1, Payload: []byte{0xff, 0xff, 0xff, 0xff}}
	if _, err := DecodeBatchBegin(frame); err == nil {
		t.Fatal("DecodeBatchBegin accepted impossible count")
	}
}

func TestBatchCodecRejectsDuplicateAndAcceptsEmpty(t *testing.T) {
	oid := testOID(20, 1)
	if _, err := BatchBeginFrame(BatchRequest{ID: 1, Commits: []OID{oid, bytes.Clone(oid)}}); err == nil {
		t.Fatal("BatchBeginFrame accepted duplicate OIDs")
	}
	var payload []byte
	payload = binary.BigEndian.AppendUint32(payload, 2)
	payload = appendBytes(payload, oid)
	payload = appendBytes(payload, oid)
	if _, err := DecodeBatchBegin(Frame{Tag: TagBatchBegin, BatchID: 1, Payload: payload}); err == nil {
		t.Fatal("DecodeBatchBegin accepted duplicate OIDs")
	}
	frame, err := BatchBeginFrame(BatchRequest{ID: 2})
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeBatchBegin(frame)
	if err != nil || request.ID != 2 || len(request.Commits) != 0 {
		t.Fatalf("empty request=%#v error=%v", request, err)
	}
}

func TestCanonicalMultisetRetainsMultiplicity(t *testing.T) {
	record := testRecords(20)[0]
	if err := CompareMultisets([]Record{record, record}, []Record{record}); err == nil {
		t.Fatal("multiset comparison hid duplicate")
	}
	if err := CompareMultisets([]Record{testRecords(20)[1], record}, []Record{record, testRecords(20)[1]}); err != nil {
		t.Fatalf("order-independent comparison failed: %v", err)
	}
}

func TestHunkContinuationOverSixteenMiB(t *testing.T) {
	added := bytes.Repeat([]byte("0123456789abcdef"), (17<<20)/16+1)
	added = added[:17<<20]
	want := HunkRecord{
		Commit: testOID(20, 1), NewPath: []byte("large-line"), NewPosition: 99,
		Added: added, MissingFinalNewline: true,
	}
	frames, err := HunkFrames(12, want, DefaultMaxFrame)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 4 || frames[0].Tag != TagHunkBegin || frames[len(frames)-1].Tag != TagHunkEnd {
		t.Fatalf("continuation tags = %#v", frames)
	}
	got, err := DecodeHunkBegin(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames[1 : len(frames)-1] {
		if frame.Tag != TagHunkAdd || uint64(12+len(frame.Payload)) > uint64(DefaultMaxFrame) {
			t.Fatalf("invalid HADD frame tag=%q size=%d", frame.Tag, 12+len(frame.Payload))
		}
		got.Added = append(got.Added, frame.Payload...)
	}
	got.MissingFinalNewline, err = DecodeHunkEnd(frames[len(frames)-1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Added, want.Added) || got.NewPosition != want.NewPosition || got.MissingFinalNewline != want.MissingFinalNewline {
		t.Fatal("continued hunk differs after assembly")
	}
}

func TestHunkEndRejectsMalformedBoolean(t *testing.T) {
	tests := []Frame{
		{Tag: TagHunkEnd, BatchID: 1},
		{Tag: TagHunkEnd, BatchID: 1, Payload: []byte{2}},
		{Tag: TagHunkEnd, BatchID: 1, Payload: []byte{0, 1}},
	}
	for _, frame := range tests {
		if _, err := DecodeHunkEnd(frame); err == nil {
			t.Fatalf("DecodeHunkEnd accepted payload %v", frame.Payload)
		}
	}
}

func TestHunkContinuationMetadataBound(t *testing.T) {
	_, err := HunkFrames(1, HunkRecord{Commit: testOID(20, 1), NewPath: bytes.Repeat([]byte{'p'}, 256), Added: []byte("large")}, 64)
	if err == nil {
		t.Fatal("HunkFrames accepted oversized HBGN metadata")
	}
}

func TestHunkContinuationRequiresAddedPayload(t *testing.T) {
	_, err := HunkFrames(1, HunkRecord{
		Commit:  testOID(20, 1),
		NewPath: bytes.Repeat([]byte{'p'}, 48),
	}, 64)
	if err == nil {
		t.Fatal("HunkFrames emitted HBGN/HEND without HADD")
	}
}

func TestErrorCodecSupportsPreflightUnsupported(t *testing.T) {
	want := &EngineError{Kind: ErrorUnsupported, BatchID: 0, Op: "preflight", Err: errors.New("textconv configured")}
	got, err := DecodeError(ErrorFrame(want.BatchID, want.Kind, want.Op, want.Err))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || got.BatchID != want.BatchID || got.Op != want.Op || got.Err.Error() != want.Err.Error() {
		t.Fatalf("DecodeError = %#v, want %#v", got, want)
	}
}

func FuzzFrameAndRecordCodec(f *testing.F) {
	for _, record := range append(testRecords(20), testRecords(32)...) {
		frame, _ := RecordFrame(1, record)
		var wire bytes.Buffer
		_ = WriteFrame(&wire, frame, 0)
		f.Add(wire.Bytes())
	}
	f.Add([]byte{0, 0, 0, 12, 'H', 'U', 'N', 'K'})
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, err := ReadFrame(bytes.NewReader(data), 1<<20)
		if err != nil {
			return
		}
		record, err := DecodeRecord(frame)
		if err != nil {
			return
		}
		reencoded, err := RecordFrame(frame.BatchID, record)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := CanonicalRecord(record)
		roundTrip, err := DecodeRecord(reencoded)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := CanonicalRecord(roundTrip)
		if !bytes.Equal(got, want) {
			t.Fatal("canonical round trip differs")
		}
	})
}

func FuzzHunkFrames(f *testing.F) {
	f.Add([]byte("path"), []byte("added\x00bytes"), uint64(1), true)
	f.Add([]byte("p"), bytes.Repeat([]byte{'x'}, 512), uint64(99), false)
	f.Fuzz(func(t *testing.T, path, added []byte, position uint64, missing bool) {
		if bytes.IndexByte(path, 0) >= 0 || len(path) > 4096 || len(added) > 1<<20 {
			return
		}
		want := HunkRecord{
			Commit: testOID(20, 1), NewPath: path, NewPosition: position,
			Added: added, MissingFinalNewline: missing,
		}
		frames, err := HunkFrames(1, want, 256)
		if err != nil {
			return
		}
		var got HunkRecord
		if len(frames) == 1 {
			record, err := DecodeRecord(frames[0])
			if err != nil {
				t.Fatal(err)
			}
			got = record.Hunk
		} else {
			got, err = DecodeHunkBegin(frames[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, frame := range frames[1 : len(frames)-1] {
				if frame.Tag != TagHunkAdd {
					t.Fatalf("continuation tag = %q", frame.Tag)
				}
				got.Added = append(got.Added, frame.Payload...)
			}
			got.MissingFinalNewline, err = DecodeHunkEnd(frames[len(frames)-1])
			if err != nil {
				t.Fatal(err)
			}
		}
		gotCanonical, err := CanonicalRecord(Record{Kind: RecordHunk, Hunk: got})
		if err != nil {
			t.Fatal(err)
		}
		wantCanonical, err := CanonicalRecord(Record{Kind: RecordHunk, Hunk: want})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gotCanonical, wantCanonical) {
			t.Fatal("hunk continuation round trip differs")
		}
	})
}
