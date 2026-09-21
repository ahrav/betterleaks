package detect

import (
	"github.com/betterleaks/betterleaks/v2/config"
	"github.com/betterleaks/betterleaks/v2/regexp"
)

// compiledRule owns the runtime regexes for an immutable snapshot of a rule.
// Regex backends are initialized lazily, unless precompilation is requested.
type compiledRule struct {
	rule  config.Rule
	regex *regexp.Regexp
	path  *regexp.Regexp

	// index is the rule's position in Detector.rulesBySpecificity.
	index int
	// anchored reports that every regex match begins with one of the rule's
	// leading literals, which the keyword automaton locates; see anchor.go.
	anchored bool
}
