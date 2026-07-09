package detect

import (
	"context"
	"fmt"
	"os"
	"regexp/syntax"
	"runtime"
	"slices"
	"testing"

	"github.com/fatih/semgroup"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/sources"
)

func TestMandatoryAtomInventory(t *testing.T) {
	if os.Getenv("BETTERLEAKS_MANDATORY_ATOM_INVENTORY") == "" {
		t.Skip("set BETTERLEAKS_MANDATORY_ATOM_INVENTORY=1 to inventory mandatory regex atoms")
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}

	rulesWithRegex := 0
	rulesWithAtoms := 0
	uncoveredRules := 0
	uncoveredAtoms := 0
	for _, ruleID := range cfg.OrderedRules {
		rule, ok := cfg.Rules[ruleID]
		if !ok || rule.Regex == nil {
			continue
		}
		rulesWithRegex++
		parsed, err := syntax.Parse(rule.Regex.String(), syntax.Perl)
		if err != nil {
			t.Fatalf("parse %s: %v", ruleID, err)
		}
		atoms := requiredLiteralAtoms(parsed.Simplify())
		atoms = usefulMandatoryAtoms(atoms)
		if len(atoms) == 0 {
			continue
		}
		rulesWithAtoms++

		var uncovered []string
		for _, atom := range atoms {
			if !atomCoveredByKeywords(atom, rule.Keywords) {
				uncovered = append(uncovered, atom)
			}
		}
		if len(uncovered) == 0 {
			continue
		}
		uncoveredRules++
		uncoveredAtoms += len(uncovered)
		fmt.Printf("ATOM rule=%s uncovered=%q keywords=%q\n", ruleID, uncovered, rule.Keywords)
	}

	fmt.Printf("METRIC regex_rules=%d\n", rulesWithRegex)
	fmt.Printf("METRIC rules_with_required_atoms=%d\n", rulesWithAtoms)
	fmt.Printf("METRIC rules_with_uncovered_required_atoms=%d\n", uncoveredRules)
	fmt.Printf("METRIC uncovered_required_atoms=%d\n", uncoveredAtoms)
}

func TestMandatoryAtomOpportunityForPath(t *testing.T) {
	path := os.Getenv("BETTERLEAKS_MANDATORY_ATOM_PATH")
	if path == "" {
		t.Skip("set BETTERLEAKS_MANDATORY_ATOM_PATH to measure mandatory atom gate opportunity")
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	atomsByRule := uncoveredMandatoryAtomsByRule(t, cfg)

	ctx := context.Background()
	d := NewDetector(cfg)
	src := &sources.Files{
		Path: path,
		Sema: semgroup.NewGroup(ctx, int64(max(1, runtime.NumCPU()))),
	}

	var fragments, candidateChecks, atomRuleChecks, atomRejects, atomAccepts, falseRejects uint64
	err = src.Fragments(ctx, func(fragment sources.Fragment, err error) error {
		if err != nil {
			return err
		}
		if fragment.Raw == "" {
			return nil
		}
		fragments++
		scratch := d.candidatePool.Get().(*candidateScratch)
		lowerBufPtr, lowerBuf := getLowerBuf(fragment.Raw)
		d.prefilter.Walk(lowerBuf, func(end, n, pattern uint32) bool {
			for _, rank := range d.prefilterRuleRanks[pattern] {
				if !scratch.seen[rank] {
					scratch.seen[rank] = true
					scratch.ranks = append(scratch.ranks, rank)
				}
			}
			return true
		})
		for _, rank := range d.noKeywordRuleRanks {
			if !scratch.seen[rank] {
				scratch.seen[rank] = true
				scratch.ranks = append(scratch.ranks, rank)
			}
		}
		slices.Sort(scratch.ranks)
		for _, rank := range scratch.ranks {
			candidateChecks++
			ruleID := d.rulesBySpecificity[rank]
			atoms := atomsByRule[ruleID]
			if len(atoms) == 0 {
				continue
			}
			atomRuleChecks++
			if mandatoryAtomsPresent(lowerBuf, atoms) {
				atomAccepts++
				continue
			}
			atomRejects++
			rule := cfg.Rules[ruleID]
			if rule.Regex != nil && rule.Regex.MatchString(fragment.Raw) {
				falseRejects++
				fmt.Printf("FALSE_REJECT rule=%s path=%s atoms=%q\n", ruleID, fragment.Attr(sources.AttrPath), atoms)
			}
		}
		putLowerBuf(lowerBufPtr)
		scratch.reset()
		d.candidatePool.Put(scratch)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if falseRejects != 0 {
		t.Fatalf("mandatory atom simulation had %d false rejects", falseRejects)
	}

	fmt.Printf("METRIC mandatory_atom_rules=%d\n", len(atomsByRule))
	fmt.Printf("METRIC fragments=%d\n", fragments)
	fmt.Printf("METRIC candidate_rule_checks=%d\n", candidateChecks)
	fmt.Printf("METRIC atom_rule_checks=%d\n", atomRuleChecks)
	fmt.Printf("METRIC atom_rejects=%d\n", atomRejects)
	fmt.Printf("METRIC atom_accepts=%d\n", atomAccepts)
	fmt.Printf("METRIC atom_false_rejects=%d\n", falseRejects)
}

func TestMinimumLengthOpportunityForPath(t *testing.T) {
	path := os.Getenv("BETTERLEAKS_MIN_LENGTH_PATH")
	if path == "" {
		t.Skip("set BETTERLEAKS_MIN_LENGTH_PATH to measure minimum-length gate opportunity")
	}

	cfg, err := config.Default()
	if err != nil {
		t.Fatal(err)
	}
	minLenByRule := minimumLengthByRule(t, cfg)

	ctx := context.Background()
	d := NewDetector(cfg)
	src := &sources.Files{
		Path: path,
		Sema: semgroup.NewGroup(ctx, int64(max(1, runtime.NumCPU()))),
	}

	var fragments, candidateChecks, minRuleChecks, minRejects uint64
	err = src.Fragments(ctx, func(fragment sources.Fragment, err error) error {
		if err != nil {
			return err
		}
		if fragment.Raw == "" {
			return nil
		}
		fragments++
		scratch := d.candidatePool.Get().(*candidateScratch)
		lowerBufPtr, lowerBuf := getLowerBuf(fragment.Raw)
		d.prefilter.Walk(lowerBuf, func(end, n, pattern uint32) bool {
			for _, rank := range d.prefilterRuleRanks[pattern] {
				if !scratch.seen[rank] {
					scratch.seen[rank] = true
					scratch.ranks = append(scratch.ranks, rank)
				}
			}
			return true
		})
		for _, rank := range d.noKeywordRuleRanks {
			if !scratch.seen[rank] {
				scratch.seen[rank] = true
				scratch.ranks = append(scratch.ranks, rank)
			}
		}
		for _, rank := range scratch.ranks {
			candidateChecks++
			ruleID := d.rulesBySpecificity[rank]
			minLen := minLenByRule[ruleID]
			if minLen == 0 {
				continue
			}
			minRuleChecks++
			if len(fragment.Raw) < minLen {
				minRejects++
			}
		}
		putLowerBuf(lowerBufPtr)
		scratch.reset()
		d.candidatePool.Put(scratch)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	fmt.Printf("METRIC min_length_rules=%d\n", len(minLenByRule))
	fmt.Printf("METRIC fragments=%d\n", fragments)
	fmt.Printf("METRIC candidate_rule_checks=%d\n", candidateChecks)
	fmt.Printf("METRIC min_length_rule_checks=%d\n", minRuleChecks)
	fmt.Printf("METRIC min_length_rejects=%d\n", minRejects)
}

func uncoveredMandatoryAtomsByRule(t *testing.T, cfg *config.Config) map[string][][]byte {
	t.Helper()
	atomsByRule := make(map[string][][]byte)
	for _, ruleID := range cfg.OrderedRules {
		rule, ok := cfg.Rules[ruleID]
		if !ok || rule.Regex == nil {
			continue
		}
		if atoms := mandatoryAtomGateForPattern(rule.Regex.String(), rule.Keywords); len(atoms) > 0 {
			atomsByRule[ruleID] = atoms
		}
	}
	return atomsByRule
}

func minimumLengthByRule(t *testing.T, cfg *config.Config) map[string]int {
	t.Helper()
	minLenByRule := make(map[string]int)
	for _, ruleID := range cfg.OrderedRules {
		rule, ok := cfg.Rules[ruleID]
		if !ok || rule.Regex == nil {
			continue
		}
		parsed, err := syntax.Parse(rule.Regex.String(), syntax.Perl)
		if err != nil {
			t.Fatalf("parse %s: %v", ruleID, err)
		}
		minLen := minimumMatchLength(parsed.Simplify())
		if minLen > 0 {
			minLenByRule[ruleID] = minLen
		}
	}
	return minLenByRule
}

func minimumMatchLength(re *syntax.Regexp) int {
	if re == nil {
		return 0
	}
	switch re.Op {
	case syntax.OpNoMatch:
		return 1 << 30
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary,
		syntax.OpNoWordBoundary:
		return 0
	case syntax.OpLiteral:
		return len(string(re.Rune))
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return 1
	case syntax.OpCapture:
		if len(re.Sub) == 0 {
			return 0
		}
		return minimumMatchLength(re.Sub[0])
	case syntax.OpStar, syntax.OpQuest:
		return 0
	case syntax.OpPlus:
		if len(re.Sub) == 0 {
			return 0
		}
		return minimumMatchLength(re.Sub[0])
	case syntax.OpRepeat:
		if len(re.Sub) == 0 || re.Min <= 0 {
			return 0
		}
		return re.Min * minimumMatchLength(re.Sub[0])
	case syntax.OpConcat:
		total := 0
		for _, sub := range re.Sub {
			total += minimumMatchLength(sub)
		}
		return total
	case syntax.OpAlternate:
		if len(re.Sub) == 0 {
			return 0
		}
		minLen := minimumMatchLength(re.Sub[0])
		for _, sub := range re.Sub[1:] {
			minLen = min(minLen, minimumMatchLength(sub))
		}
		return minLen
	default:
		return 0
	}
}
