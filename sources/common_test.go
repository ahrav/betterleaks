package sources

import (
	"bufio"
	"bytes"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_readUntilSafeBoundary(t *testing.T) {
	// Arrange
	cases := []struct {
		name     string
		r        io.Reader
		expected string
	}{
		// Current split is fine, exit early.
		{
			name:     "safe original split - LF",
			r:        strings.NewReader("abc\n\ndefghijklmnop\n\nqrstuvwxyz"),
			expected: "abc\n\n",
		},
		{
			name:     "safe original split - CRLF",
			r:        strings.NewReader("a\r\n\r\nbcdefghijklmnop\n"),
			expected: "a\r\n\r\n",
		},
		// Current split is bad, look for a better one.
		{
			name:     "safe split - LF",
			r:        strings.NewReader("abcdefg\nhijklmnop\n\nqrstuvwxyz"),
			expected: "abcdefg\nhijklmnop\n\n",
		},
		{
			name:     "safe split - CRLF",
			r:        strings.NewReader("abcdefg\r\nhijklmnop\r\n\r\nqrstuvwxyz"),
			expected: "abcdefg\r\nhijklmnop\r\n\r\n",
		},
		{
			name:     "safe split - blank line",
			r:        strings.NewReader("abcdefg\nhijklmnop\n\t  \t\nqrstuvwxyz"),
			expected: "abcdefg\nhijklmnop\n\t  \t\n",
		},
		// Current split is bad, exhaust options.
		{
			name:     "no safe split",
			r:        strings.NewReader("abcdefg\nhijklmnopqrstuvwxyz"),
			expected: "abcdefg\nhijklmnopqrstuvwx",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf := make([]byte, 5)
			n, err := c.r.Read(buf)
			require.NoError(t, err)

			// Act
			reader := bufio.NewReader(c.r)
			peekBuf := bytes.NewBuffer(buf[:n])
			err = readUntilSafeBoundary(reader, n, 20, peekBuf)
			require.NoError(t, err)

			// Assert
			t.Log(peekBuf.String())
			require.Equal(t, c.expected, peekBuf.String())
		})
	}
}

func TestReadUntilSafeBoundaryMatchesBytewiseOracle(t *testing.T) {
	random := rand.New(rand.NewSource(0x5eed))
	alphabet := []byte("abcdef0123456789 \t\r\n")
	for iteration := range 5_000 {
		length := 1 + random.Intn(2_048)
		content := make([]byte, length)
		for i := range content {
			content[i] = alphabet[random.Intn(len(alphabet))]
		}
		initialLength := 1 + random.Intn(length)
		maxPeek := random.Intn(512)
		readerSize := 16 << random.Intn(5)

		oracleReader := bufio.NewReaderSize(bytes.NewReader(content[initialLength:]), readerSize)
		optimizedReader := bufio.NewReaderSize(bytes.NewReader(content[initialLength:]), readerSize)
		oracleBuffer := bytes.NewBuffer(append([]byte(nil), content[:initialLength]...))
		optimizedBuffer := bytes.NewBuffer(append([]byte(nil), content[:initialLength]...))

		if err := readUntilSafeBoundaryBytewise(oracleReader, initialLength, maxPeek, oracleBuffer); err != nil {
			t.Fatalf("iteration %d: oracle error: %v", iteration, err)
		}
		if err := readUntilSafeBoundary(optimizedReader, initialLength, maxPeek, optimizedBuffer); err != nil {
			t.Fatalf("iteration %d: optimized error: %v", iteration, err)
		}
		if !bytes.Equal(optimizedBuffer.Bytes(), oracleBuffer.Bytes()) {
			t.Fatalf(
				"iteration %d: optimized fragment differs from oracle\noracle=%q\noptimized=%q",
				iteration,
				oracleBuffer.Bytes(),
				optimizedBuffer.Bytes(),
			)
		}

		oracleRemainder, err := io.ReadAll(oracleReader)
		if err != nil {
			t.Fatalf("iteration %d: read oracle remainder: %v", iteration, err)
		}
		optimizedRemainder, err := io.ReadAll(optimizedReader)
		if err != nil {
			t.Fatalf("iteration %d: read optimized remainder: %v", iteration, err)
		}
		if !bytes.Equal(optimizedRemainder, oracleRemainder) {
			t.Fatalf(
				"iteration %d: optimized consumption differs from oracle\noracle=%q\noptimized=%q",
				iteration,
				oracleRemainder,
				optimizedRemainder,
			)
		}
	}
}

func readUntilSafeBoundaryBytewise(r *bufio.Reader, n int, maxPeekSize int, peekBuf *bytes.Buffer) error {
	if peekBuf.Len() == 0 {
		return nil
	}

	data := peekBuf.Bytes()
	lastChar := data[len(data)-1]
	newlineCount := 0
	if isWhitespace[lastChar] {
		for i := len(data) - 1; i >= 0; i-- {
			lastChar = data[i]
			if lastChar == '\n' {
				newlineCount++
				if newlineCount >= 2 {
					return nil
				}
			} else if !isWhitespace[lastChar] {
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
		} else if !isWhitespace[lastChar] {
			newlineCount = 0
		}
		if peekBuf.Len()-n >= maxPeekSize {
			break
		}
		b, err := r.ReadByte()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		_ = peekBuf.WriteByte(b)
	}
	return nil
}
