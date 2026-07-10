package detect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/gitleaks/go-gitdiff/gitdiff"
	"github.com/stretchr/testify/require"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/sources"
)

// goldenFindingsDigest pins the end-to-end findings of the default config
// over a committed git-log patch stream (sources/testdata/digest_fixture.patch,
// captured from this repository's own history). This is the CI incarnation
// of the digest guard that gated every perf commit: any change to parsing,
// prefiltering, gating, or detection that alters WHICH findings are
// reported — not just their order — changes this hash.
//
// To update after an intentional behavior change: run the test, verify the
// printed findings diff is expected, and paste the new digest.
const goldenFindingsDigest = "e1502267a2929cbfb3ad81ed02c56d7d635b4d28a74b44eea15eec42ebdb867d"

func TestFindingsDigestGolden(t *testing.T) {
	patch, err := os.ReadFile("../sources/testdata/digest_fixture.patch")
	require.NoError(t, err)

	cfg, err := config.Default()
	require.NoError(t, err)
	d := NewDetector(cfg)

	// Feed the fixture through the same parser log mode uses, then detect
	// per file exactly as sources.Git.Fragments does (added lines only).
	files, err := sources.ParseGitLogStreamForTest(patch)
	require.NoError(t, err)

	var keys []string
	for _, file := range files {
		if file.IsDelete || file.IsBinary {
			continue
		}
		for _, tf := range file.TextFragments {
			if tf == nil {
				continue
			}
			frag := sources.Fragment{
				Raw:       tf.Raw(gitdiff.OpAdd),
				StartLine: int(tf.NewPosition),
				Attributes: map[string]string{
					sources.AttrPath: file.NewName,
				},
			}
			if file.PatchHeader != nil {
				frag.Attributes[sources.AttrGitSHA] = file.PatchHeader.SHA
			}
			for _, f := range d.Detect(frag) {
				keys = append(keys, fmt.Sprintf("%s|%s|%d|%s|%s",
					f.RuleID, f.Attr(sources.AttrPath), f.StartLine, f.Secret, f.Attr(sources.AttrGitSHA)))
			}
		}
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	digest := hex.EncodeToString(h.Sum(nil))

	if goldenFindingsDigest == "REPLACE_ME" {
		t.Fatalf("golden digest not set; found %d findings, digest %s\nfindings:\n%s",
			len(keys), digest, joinLines(keys))
	}
	if digest != goldenFindingsDigest {
		t.Fatalf("findings digest changed: got %s want %s\n%d findings:\n%s",
			digest, goldenFindingsDigest, len(keys), joinLines(keys))
	}
}

func joinLines(ss []string) string {
	out := ""
	for _, s := range ss {
		out += "  " + s + "\n"
	}
	return out
}
