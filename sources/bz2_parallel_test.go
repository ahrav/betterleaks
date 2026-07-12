package sources

import (
	"bytes"
	"compress/bzip2"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBz2ParallelDecodeEquivalence scans identical content as (a) plain
// file and (b) bzip2-compressed file, requiring the same fragment bytes,
// and separately verifies the parallel reader round-trips multi-block
// archives byte-identically vs the stdlib decoder.
func TestBz2ParallelDecodeEquivalence(t *testing.T) {
	if _, err := exec.LookPath("bzip2"); err != nil {
		t.Skip("bzip2 binary not available")
	}
	dir := t.TempDir()

	// >900KB forces multiple bzip2 blocks at default block size 900k...
	// actually bzip2 -1 uses 100k blocks; use -1 to force many blocks.
	var content bytes.Buffer
	for content.Len() < 3<<20 {
		content.WriteString("some source line with token = \"value\" and assorted text\n")
		content.WriteString("aws_key = \"AKIAIOSFODNN7EXAMPLE\"\n")
	}
	plainPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(plainPath, content.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bzip2", "-1", "-k", plainPath).CombinedOutput(); err != nil {
		t.Fatalf("bzip2: %v: %s", err, out)
	}
	bz2Path := plainPath + ".bz2"

	// Round-trip check: parallel path output == stdlib bzip2 output.
	bz2Data, err := os.ReadFile(bz2Path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(bz2Data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, content.Bytes()) {
		t.Fatal("stdlib round-trip mismatch (test setup broken)")
	}

	// Fragment boundaries follow the underlying reader's Read granularity
	// and legitimately differ across decoders; the invariant is that the
	// fragments are sequential, disjoint, and reconstruct the content.
	reconstruct := func(path string) string {
		ctx := context.Background()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		src := &File{
			Content:         f,
			Path:            path,
			MaxArchiveDepth: 2,
		}
		var sb bytes.Buffer
		err = src.Fragments(ctx, func(f Fragment, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			sb.WriteString(f.Raw)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return sb.String()
	}

	plainContent := reconstruct(plainPath)
	bz2Content := reconstruct(bz2Path)
	if plainContent != bz2Content {
		t.Fatalf("reconstructed content differs: plain=%d bytes bz2=%d bytes", len(plainContent), len(bz2Content))
	}
	if plainContent != content.String() {
		t.Fatalf("reconstructed content differs from source: %d vs %d bytes", len(plainContent), content.Len())
	}
}
