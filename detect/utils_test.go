package detect

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/betterleaks/betterleaks/report"
)

func TestSamePath(t *testing.T) {
	// A native-separator config path and the forward-slash fragment path the
	// file source produces must compare equal. The bug only bit on Windows,
	// where filepath.FromSlash yields backslashes.
	cfg := filepath.FromSlash("proj/sub/.betterleaks.toml")
	assert.True(t, samePath("proj/sub/.betterleaks.toml", cfg))
	assert.False(t, samePath("proj/sub/other.toml", cfg))
}

func Test_createScmLink(t *testing.T) {
	tests := map[string]struct {
		platform  string
		remoteURL string
		finding   report.Finding
		want      string
	}{
		// None
		"no platform": {
			platform:  "none",
			remoteURL: "",
			want:      "",
		},

		// GitHub
		"github - single line": {
			platform:  "github",
			remoteURL: "https://github.com/gitleaks/test",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "metrics/% of sales/.env",
				StartLine: 25,
				EndLine:   25,
			},
			want: "https://github.com/gitleaks/test/blob/20553ad96a4a080c94a54d677db97eed8ce2560d/metrics/%25%20of%20sales/.env#L25",
		},
		"github - multi line": {
			platform:  "github",
			remoteURL: "https://github.com/gitleaks/test",
			finding: report.Finding{
				Commit:    "7bad9f7654cf9701b62400281748c0e8efd97666",
				File:      "config.json",
				StartLine: 235,
				EndLine:   238,
			},
			want: "https://github.com/gitleaks/test/blob/7bad9f7654cf9701b62400281748c0e8efd97666/config.json#L235-L238",
		},
		"github - markdown": {
			platform:  "github",
			remoteURL: "https://github.com/gitleaks/test",
			finding: report.Finding{
				Commit:    "1fc8961d172f39ffb671766e472aa76f8d713e87",
				File:      "docs/guides/ecosystem/discordjs.MD",
				StartLine: 34,
				EndLine:   34,
			},
			want: "https://github.com/gitleaks/test/blob/1fc8961d172f39ffb671766e472aa76f8d713e87/docs/guides/ecosystem/discordjs.MD?plain=1#L34",
		},
		"github - jupyter notebook": {
			platform:  "github",
			remoteURL: "https://github.com/gitleaks/test",
			finding: report.Finding{
				Commit:    "8f56bd2369595bcadbb007e88ba294630fb05c7b",
				File:      "Cloud/IPYNB/Overlapping Recommendation algorithm _OCuLaR_.ipynb",
				StartLine: 293,
				EndLine:   293,
			},
			want: "https://github.com/gitleaks/test/blob/8f56bd2369595bcadbb007e88ba294630fb05c7b/Cloud/IPYNB/Overlapping%20Recommendation%20algorithm%20_OCuLaR_.ipynb?plain=1#L293",
		},

		// GitLab
		"gitlab - single line": {
			platform:  "gitlab",
			remoteURL: "https://gitlab.com/example-org/example-group/gitleaks",
			finding: report.Finding{
				Commit:    "213ffd1c9bfa906eb4c7731771132c58a4ca0139",
				File:      ".gitlab-ci.yml",
				StartLine: 41,
				EndLine:   41,
			},
			want: "https://gitlab.com/example-org/example-group/gitleaks/blob/213ffd1c9bfa906eb4c7731771132c58a4ca0139/.gitlab-ci.yml#L41",
		},
		"gitlab - multi line": {
			platform:  "gitlab",
			remoteURL: "https://gitlab.com/example-org/example-group/gitleaks",
			finding: report.Finding{
				Commit:    "63410f74e23a4e51e1f60b9feb073b5d325af878",
				File:      ".vscode/launchSettings.json",
				StartLine: 6,
				EndLine:   8,
			},
			want: "https://gitlab.com/example-org/example-group/gitleaks/blob/63410f74e23a4e51e1f60b9feb073b5d325af878/.vscode/launchSettings.json#L6-8",
		},

		// Azure DevOps
		"azuredevops - single line": {
			platform:  "azuredevops",
			remoteURL: "https://dev.azure.com/exampleorganisation/exampleproject/_git/exampleRepository",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   25,
			},
			want: "https://dev.azure.com/exampleorganisation/exampleproject/_git/exampleRepository/commit/20553ad96a4a080c94a54d677db97eed8ce2560d?path=/examplefile.json&line=25&lineStartColumn=1&lineEndColumn=10000000&type=2&lineStyle=plain&_a=files",
		},

		// Azure DevOps
		"azuredevops - multi line": {
			platform:  "azuredevops",
			remoteURL: "https://dev.azure.com/exampleorganisation/exampleproject/_git/exampleRepository",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   30,
			},
			want: "https://dev.azure.com/exampleorganisation/exampleproject/_git/exampleRepository/commit/20553ad96a4a080c94a54d677db97eed8ce2560d?path=/examplefile.json&line=25&lineEnd=30&lineStartColumn=1&lineEndColumn=10000000&type=2&lineStyle=plain&_a=files",
		},

		// Gitea
		"gitea - single line": {
			platform:  "gitea",
			remoteURL: "https://gitea.com/exampleorganisation/exampleproject",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   25,
			},
			want: "https://gitea.com/exampleorganisation/exampleproject/src/commit/20553ad96a4a080c94a54d677db97eed8ce2560d/examplefile.json#L25",
		},
		"gitea- multi line": {
			platform:  "gitea",
			remoteURL: "https://gitea.com/exampleorganisation/exampleproject",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   30,
			},
			want: "https://gitea.com/exampleorganisation/exampleproject/src/commit/20553ad96a4a080c94a54d677db97eed8ce2560d/examplefile.json#L25-L30",
		},
		"gitea - markdown": {
			platform:  "gitea",
			remoteURL: "https://gitea.com/exampleorganisation/exampleproject",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "Readme.md",
				StartLine: 34,
				EndLine:   34,
			},
			want: "https://gitea.com/exampleorganisation/exampleproject/src/commit/20553ad96a4a080c94a54d677db97eed8ce2560d/Readme.md?display=source#L34",
		},
		// bitbucket
		"bitbucket - single line": {
			platform:  "bitbucket",
			remoteURL: "https://bitbucket.org/exampleorganisation/exampleproject",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   25,
			},
			want: "https://bitbucket.org/exampleorganisation/exampleproject/src/20553ad96a4a080c94a54d677db97eed8ce2560d/examplefile.json#lines-25",
		},
		"bitbucket- multi line": {
			platform:  "bitbucket",
			remoteURL: "https://bitbucket.org/exampleorganisation/exampleproject",
			finding: report.Finding{
				Commit:    "20553ad96a4a080c94a54d677db97eed8ce2560d",
				File:      "examplefile.json",
				StartLine: 25,
				EndLine:   30,
			},
			want: "https://bitbucket.org/exampleorganisation/exampleproject/src/20553ad96a4a080c94a54d677db97eed8ce2560d/examplefile.json#lines-25:30",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			actual := createScmLink(tt.platform, tt.remoteURL, tt.finding)
			assert.Equal(t, tt.want, actual)
		})
	}
}

// scalarASCIILower is the obviously-correct reference the SWAR asciiLower must
// match byte-for-byte.
func scalarASCIILower(dst []byte, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			dst[i] = c + 32
		} else {
			dst[i] = c
		}
	}
}

// TestAsciiLowerEquivalence proves the branchless SWAR asciiLower is byte-exact
// against the scalar reference. It stresses the two failure modes of SWAR byte
// arithmetic: cross-byte borrow/carry (adjacency sweep) and the ASCII boundary
// (all 256 byte values, including >= 0x80 which must pass through unchanged).
func TestAsciiLowerEquivalence(t *testing.T) {
	check := func(in []byte) {
		want := make([]byte, len(in))
		got := make([]byte, len(in))
		scalarASCIILower(want, string(in))
		asciiLower(got, string(in))
		assert.Equal(t, want, got, "input %x", in)
	}

	// Every byte value, alone and length-padded, exercising all 8 lane offsets
	// plus the scalar remainder for non-multiple-of-8 lengths.
	for v := 0; v < 256; v++ {
		for _, n := range []int{1, 7, 8, 9, 15, 16, 17} {
			for off := 0; off < n; off++ {
				buf := make([]byte, n)
				for k := range buf {
					buf[k] = 0xFF // hostile background stresses borrow propagation
				}
				buf[off] = byte(v)
				check(buf)
			}
		}
	}

	// All adjacency pairs at every offset in a full 8-byte word: catches any
	// borrow leaking from one byte's range test into its neighbour.
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			for pos := 0; pos < 7; pos++ {
				buf := make([]byte, 8)
				buf[pos] = byte(a)
				buf[pos+1] = byte(b)
				check(buf)
			}
		}
	}

	// Empty input must be a no-op.
	check(nil)
}
