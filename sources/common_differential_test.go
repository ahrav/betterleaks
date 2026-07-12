package sources

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// readUntilSafeBoundaryReference is the historical byte-at-a-time
// implementation, kept verbatim as the differential oracle for the
// chunked rewrite.
func readUntilSafeBoundaryReference(r *bufio.Reader, n int, maxPeekSize int, peekBuf *bytes.Buffer) error {
	if peekBuf.Len() == 0 {
		return nil
	}

	var (
		data         = peekBuf.Bytes()
		lastChar     = data[len(data)-1]
		newlineCount = 0
	)

	if isWhitespace[lastChar] {
		for i := len(data) - 1; i >= 0; i-- {
			lastChar = data[i]
			if lastChar == '\n' {
				newlineCount++
				if newlineCount >= 2 {
					return nil
				}
			} else if isWhitespace[lastChar] {
			} else {
				break
			}
		}
	}

	newlineCount = 0
	for {
		data = peekBuf.Bytes()
		lastChar = data[len(data)-1]
		if lastChar == '\n' {
			newlineCount++
			if newlineCount >= 2 {
				break
			}
		} else if isWhitespace[lastChar] {
		} else {
			newlineCount = 0
		}

		if (peekBuf.Len() - n) >= maxPeekSize {
			break
		}

		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		peekBuf.WriteByte(b)
	}
	return nil
}

// TestReadUntilSafeBoundaryDifferential drives the chunked implementation
// and the historical reference with identical inputs — random whitespace-
// heavy streams, varied initial buffers, peek budgets, and bufio sizes —
// and requires byte-identical resulting buffers and identical remaining
// stream contents.
func TestReadUntilSafeBoundaryDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	alphabet := []byte("a\n\r\t b\n\nx\ry zz\n")

	randBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return b
	}

	for trial := 0; trial < 5000; trial++ {
		initial := randBytes(1 + rng.Intn(64))
		stream := randBytes(rng.Intn(4096))
		maxPeek := rng.Intn(2048)
		bufSize := 16 + rng.Intn(512)
		n := len(initial)

		gotBuf := bytes.NewBuffer(append([]byte{}, initial...))
		gotReader := bufio.NewReaderSize(bytes.NewReader(stream), bufSize)
		gotErr := readUntilSafeBoundary(gotReader, n, maxPeek, gotBuf)

		wantBuf := bytes.NewBuffer(append([]byte{}, initial...))
		wantReader := bufio.NewReaderSize(bytes.NewReader(stream), bufSize)
		wantErr := readUntilSafeBoundaryReference(wantReader, n, maxPeek, wantBuf)

		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("trial %d: err mismatch: got %v want %v", trial, gotErr, wantErr)
		}
		if !bytes.Equal(gotBuf.Bytes(), wantBuf.Bytes()) {
			t.Fatalf("trial %d: buffer mismatch:\ninitial=%q\nstream=%q\nmaxPeek=%d bufSize=%d\ngot= %q\nwant=%q",
				trial, initial, stream, maxPeek, bufSize, gotBuf.Bytes(), wantBuf.Bytes())
		}
		gotRest, _ := io.ReadAll(gotReader)
		wantRest, _ := io.ReadAll(wantReader)
		if !bytes.Equal(gotRest, wantRest) {
			t.Fatalf("trial %d: remaining stream mismatch: got %q want %q", trial, gotRest, wantRest)
		}
	}
}

func BenchmarkReadUntilSafeBoundary(b *testing.B) {
	// Worst case: no double newline anywhere, so the scan runs to the peek
	// budget every time. Two line-length regimes: dense 8-byte lines and
	// realistic ~60-byte source lines.
	stream := []byte(strings.Repeat("abcdefg\n", 1<<17))
	streamLong := []byte(strings.Repeat("alpha := compute(value, index) // explanatory trailing comment\n", 1<<14))
	const maxPeek = 1 << 20

	b.Run("chunked", func(b *testing.B) {
		b.SetBytes(maxPeek)
		for b.Loop() {
			buf := bytes.NewBuffer([]byte("x"))
			r := bufio.NewReaderSize(bytes.NewReader(stream), 64<<10)
			if err := readUntilSafeBoundary(r, 1, maxPeek, buf); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("chunked-long-lines", func(b *testing.B) {
		b.SetBytes(maxPeek)
		for b.Loop() {
			buf := bytes.NewBuffer([]byte("x"))
			r := bufio.NewReaderSize(bytes.NewReader(streamLong), 64<<10)
			if err := readUntilSafeBoundary(r, 1, maxPeek, buf); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("reference", func(b *testing.B) {
		b.SetBytes(maxPeek)
		for b.Loop() {
			buf := bytes.NewBuffer([]byte("x"))
			r := bufio.NewReaderSize(bytes.NewReader(stream), 64<<10)
			if err := readUntilSafeBoundaryReference(r, 1, maxPeek, buf); err != nil {
				b.Fatal(err)
			}
		}
	})
}
