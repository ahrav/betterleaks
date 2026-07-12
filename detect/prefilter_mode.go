package detect

import (
	"os"
	stdregexp "regexp"

	re2exp "github.com/betterleaks/go-re2/experimental"

	"github.com/betterleaks/betterleaks/logging"
)

// re2Set aliases the go-re2 experimental Set so detect.go stays free of the
// experimental import path.
type re2Set = re2exp.Set

// prefilterMode selects the keyword-prefilter architecture used by
// detectFragment to reduce 331 rules to a per-fragment candidate set.
// Experimental knob for the ahoc-lab study; selected once at detector
// construction from BETTERLEAKS_PREFILTER.
type prefilterMode int

const (
	// prefilterAhoC is the production default: an Aho-Corasick trie walk
	// over the lowercased fragment.
	prefilterAhoC prefilterMode = iota
	// prefilterNone disables keyword prefiltering entirely: every rule is a
	// candidate on every fragment. Rule gates (requiredAny, mandatory atom,
	// rejection regex) still run. Measures what the prefilter stage buys.
	prefilterNone
	// prefilterRE2Set replaces the Go-side Aho-Corasick walk with a single
	// RE2::Set scan (all keywords as literal alternatives, one engine call
	// per decode pass). Matched pattern indices map to rules exactly like
	// AhoC pattern indices: both report "keyword i occurs as a substring".
	prefilterRE2Set
	// prefilterTrigram replaces the Aho-Corasick walk with a rolling
	// 3-gram bitmap filter plus anchored verification; see
	// trigram_prefilter.go.
	prefilterTrigram
)

func prefilterModeFromEnv() prefilterMode {
	switch os.Getenv("BETTERLEAKS_PREFILTER") {
	case "", "ahoc":
		return prefilterAhoC
	case "none":
		return prefilterNone
	case "re2set":
		return prefilterRE2Set
	case "trigram":
		return prefilterTrigram
	default:
		logging.Fatal().Msgf("unknown BETTERLEAKS_PREFILTER %q (valid: ahoc, none, re2set, trigram)", os.Getenv("BETTERLEAKS_PREFILTER"))
		return prefilterAhoC
	}
}

// compileKeywordSet builds an RE2::Set whose pattern i is the literal
// keyword i, preserving the keyword index -> prefilterRuleRanks mapping.
func compileKeywordSet(keywords []string) *re2Set {
	exprs := make([]string, len(keywords))
	for i, kw := range keywords {
		exprs[i] = stdregexp.QuoteMeta(kw)
	}
	set, err := re2exp.CompileSet(exprs)
	if err != nil {
		logging.Fatal().Err(err).Msg("failed to compile keyword RE2 set")
	}
	return set
}
