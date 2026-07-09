package sources

import (
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeCommits returns n distinct fake SHAs whose content encodes their input
// position, so batch contents can be mapped back to input indices.
func makeCommits(n int) []string {
	commits := make([]string, n)
	for i := range commits {
		commits[i] = fmt.Sprintf("%040d", i)
	}
	return commits
}

// commitIndex inverts makeCommits.
func commitIndex(t *testing.T, sha string) int {
	t.Helper()
	var i int
	_, err := fmt.Sscanf(sha, "%d", &i)
	require.NoError(t, err)
	return i
}

// assertBatchInvariants checks the three properties buildBatches documents:
// exact multiset preservation (no commit lost or duplicated), run-contiguity
// (each batch is a concatenation of batchRunLen-aligned contiguous input
// runs), and no empty batches.
func assertBatchInvariants(t *testing.T, commits []string, batches [][]string) {
	t.Helper()

	// (a) Multiset equality: concatenation == input, no dup/loss. Commits
	// are distinct, so sorted-slice equality is multiset equality.
	var flat []string
	for _, b := range batches {
		assert.NotEmpty(t, b, "empty batch emitted")
		flat = append(flat, b...)
	}
	require.Len(t, flat, len(commits), "commit count changed: scanned set differs from input")
	sortedIn := append([]string(nil), commits...)
	sortedOut := append([]string(nil), flat...)
	sort.Strings(sortedIn)
	sort.Strings(sortedOut)
	assert.Equal(t, sortedIn, sortedOut, "batches lost or duplicated commits")

	// (b) Run contiguity: walk each batch; it must decompose into runs that
	// start at a batchRunLen-aligned input index and proceed contiguously
	// for up to batchRunLen commits (shorter only for the input tail).
	for bi, b := range batches {
		i := 0
		for i < len(b) {
			start := commitIndex(t, b[i])
			require.Zerof(t, start%batchRunLen, "batch %d: run starts at unaligned input index %d", bi, start)
			runEnd := min(start+batchRunLen, len(commits))
			for j := start; j < runEnd; j++ {
				require.Lessf(t, i, len(b), "batch %d: run starting at %d truncated mid-run", bi, start)
				require.Equalf(t, commits[j], b[i], "batch %d: run starting at %d not contiguous at offset %d", bi, start, j-start)
				i++
			}
		}
	}
}

func TestBuildBatchesInvariants(t *testing.T) {
	workersCases := []int{1, 2, 31, 32}
	countCases := []int{0, 1, 15, 16, 17, 63, 64, 65, 1000}
	for _, w := range workersCases {
		countCases = append(countCases, w*64-1, w*64, w*64+1)
	}

	for _, workers := range workersCases {
		for _, count := range countCases {
			t.Run(fmt.Sprintf("count=%d/workers=%d", count, workers), func(t *testing.T) {
				commits := makeCommits(count)
				batches := buildBatches(commits, workers)
				if count == 0 {
					assert.Empty(t, batches)
					return
				}
				require.NotEmpty(t, batches, "non-empty input produced no batches")
				assertBatchInvariants(t, commits, batches)
			})
		}
	}
}

// TestBuildBatchesDistribution pins the load-balance property that motivates
// striding: on a large history every batch stays near the target size, so no
// single git worker inherits a disproportionate share.
func TestBuildBatchesDistribution(t *testing.T) {
	const count = 100_000
	const workers = 32
	commits := makeCommits(count)
	batches := buildBatches(commits, workers)

	batchSize := max(count/(workers*8), 64)
	for bi, b := range batches {
		// Dealing whole runs means a batch can exceed the target by at most
		// one extra run per deal cycle; bound it loosely at 2x to catch
		// gross regressions (e.g. all commits landing in one batch).
		assert.LessOrEqualf(t, len(b), 2*batchSize, "batch %d grossly oversized: %d vs target %d", bi, len(b), batchSize)
	}
}
