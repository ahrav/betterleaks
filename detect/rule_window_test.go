package detect

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/report"
	"github.com/betterleaks/betterleaks/sources"
)

// TestWindowEligibleRules reports how many default rules qualify for
// hit-window verification and sanity-checks their widths.
func TestWindowEligibleRules(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	eligible, lineBounded, exempt := 0, 0, 0
	maxW := 0
	for _, rule := range cfg.Rules {
		if rule.Regex == nil {
			continue
		}
		wp := windowPlanForPattern(rule.Regex.String(), rule.Keywords)
		if wp.valid() {
			eligible++
			if wp.nlFreeSites > 0 {
				lineBounded++
			}
			if wp.bounded > maxW {
				maxW = wp.bounded
			}
		} else {
			exempt++
		}
	}
	t.Logf("window-eligible rules: %d (line-bounded: %d), exempt: %d, max static width: %d", eligible, lineBounded, exempt, maxW)
	if eligible == 0 {
		t.Fatal("no rules eligible for windowing — wiring is dead code")
	}
}

func canonicalFindings(t *testing.T, findings []report.Finding) []string {
	t.Helper()
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		f.Fingerprint = ""
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

// TestWindowedDetectionDifferential runs the full detector over adversarial
// fragments with windowing on and off, requiring byte-identical findings.
// Inputs include real-shaped secrets at fragment edges, straddling window
// margins, duplicated keywords, unicode fold traps, and random splices.
func TestWindowedDetectionDifferential(t *testing.T) {
	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	newDetector := func(disabled bool) *Detector {
		d := NewDetector(cfg)
		d.MaxDecodeDepth = 0
		d.windowingDisabled = disabled
		return d
	}

	secrets := []string{
		`aws_key = "AKIAIOSFODNN7EXAMPLE"`,
		`const Discord_Public_Key = "e7322523fb86ed64c836a979cf8465fbd436378c653c1db38f9ae87bc62a6fd5"`,
		"github_token := \"ghp_16C7e42F292c6912E7710c838347Ae178B4a\"",
		`slack_token = "xoxb-781236542736-2364535789652-GkwFDQoHqzXDVsC6GzqYUypD"`,
		`stripe_api_key: "sk_test_51H8mFbGswQrbLxN2AcTQGvVmRvhDkzPWNvJqLsIhSAcm4Kx"`,
		`generic_secret = "hjkKJHmn234$sdfHJKmnop9012jklMNOP"`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----",
	}
	fillers := []string{
		"the quick brown fox jumps over the lazy dog\n",
		"for i := range items { process(items[i]) }\n",
		"# configuration values for the deployment environment\n",
		strings.Repeat("x", 200) + "\n",
		"password username token secret apikey auth\n", // keyword soup, no secrets
	}

	check := func(name, content string) {
		t.Helper()
		frag := sources.Fragment{Raw: content, Attributes: map[string]string{sources.AttrPath: "test.py"}}
		got := canonicalFindings(t, newDetector(false).detectFragment(context.Background(), frag))
		want := canonicalFindings(t, newDetector(true).detectFragment(context.Background(), frag))
		if len(got) != len(want) {
			t.Fatalf("%s: windowed=%d findings, full=%d findings", name, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: finding %d differs:\nwindowed: %s\nfull:     %s", name, i, got[i], want[i])
			}
		}
	}

	// Each secret alone, at start, end, and deep inside filler.
	for i, sec := range secrets {
		check(fmt.Sprintf("alone-%d", i), sec)
		check(fmt.Sprintf("start-%d", i), sec+"\n"+strings.Repeat(fillers[i%len(fillers)], 30))
		check(fmt.Sprintf("end-%d", i), strings.Repeat(fillers[i%len(fillers)], 30)+sec)
		check(fmt.Sprintf("mid-%d", i), strings.Repeat(fillers[i%len(fillers)], 15)+sec+"\n"+strings.Repeat(fillers[(i+1)%len(fillers)], 15))
	}

	// All secrets in one fragment; duplicated; adjacent.
	check("all", strings.Join(secrets, "\n"))
	check("all-dup", strings.Join(secrets, "\n")+"\n"+strings.Join(secrets, "\n"))
	check("adjacent", strings.Join(secrets, ""))

	// Unicode fold traps force the full-fragment fallback path.
	check("kelvin", "Key = value\n"+secrets[0])
	check("longs", "ſecret = value\n"+secrets[1])

	// Randomized splices: secrets at random offsets in random filler.
	rng := rand.New(rand.NewSource(77))
	for trial := 0; trial < 300; trial++ {
		var sb strings.Builder
		for sb.Len() < 2000 {
			if rng.Intn(6) == 0 {
				sb.WriteString(secrets[rng.Intn(len(secrets))])
				sb.WriteByte('\n')
			}
			sb.WriteString(fillers[rng.Intn(len(fillers))])
		}
		check(fmt.Sprintf("random-%d", trial), sb.String())
	}
}
