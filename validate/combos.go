package validate

import (
	"maps"
	"sort"

	"github.com/betterleaks/betterleaks/logging"
	"github.com/betterleaks/betterleaks/report"
)

// maxCombos is the upper bound on cartesian product expansions to prevent
// excessive memory usage when many required findings share the same rule ID.
const maxCombos = 100

// Combos generates the cartesian-product combo maps for the given placeholder IDs
// and secrets. Each returned map assigns one concrete value to each ID.
// IDs not present in secrets are omitted (callers should check for missing IDs first).
func Combos(ids []string, secrets map[string][]string) []map[string]string {
	var activeIDs []string
	for _, id := range ids {
		if vals, ok := secrets[id]; ok && len(vals) > 0 {
			activeIDs = append(activeIDs, id)
		}
	}
	sort.Strings(activeIDs)

	if len(activeIDs) == 0 {
		return []map[string]string{{}}
	}
	combos := cartesian(activeIDs, secrets)
	if len(combos) > maxCombos {
		logging.Warn().
			Int("total", len(combos)).
			Int("max", maxCombos).
			Msg("validation combo count exceeds limit, truncating")
		combos = combos[:maxCombos]
	}
	return combos
}

// ExpandRequired takes required findings and returns expanded combo maps
// ready for validation submission. Each map contains ruleID->secret entries
// plus ruleID:captureName->value entries for any capture groups.
func ExpandRequired(reqs []*report.RequiredFinding) []map[string]string {
	if len(reqs) == 0 {
		return nil
	}
	ruleIDs := make([]string, 0)
	secretsByRule := make(map[string][]string)
	seen := make(map[string]struct{})
	type captureKey struct{ ruleID, secret string }
	captureIndex := make(map[captureKey]map[string]string)

	for _, req := range reqs {
		if _, ok := seen[req.RuleID]; !ok {
			seen[req.RuleID] = struct{}{}
			ruleIDs = append(ruleIDs, req.RuleID)
		}
		secretsByRule[req.RuleID] = append(secretsByRule[req.RuleID], req.Secret)
		if len(req.CaptureGroups) > 0 {
			captureIndex[captureKey{req.RuleID, req.Secret}] = req.CaptureGroups
		}
	}

	combos := Combos(ruleIDs, secretsByRule)
	result := make([]map[string]string, 0, len(combos))
	for _, combo := range combos {
		expanded := make(map[string]string, len(combo)*2)
		for ruleID, secret := range combo {
			expanded[ruleID] = secret
			if caps, ok := captureIndex[captureKey{ruleID, secret}]; ok {
				for name, val := range caps {
					expanded[ruleID+":"+name] = val
				}
			}
		}
		result = append(result, expanded)
	}
	return result
}

// cartesian produces the cartesian product of secrets for each ID.
func cartesian(ids []string, secrets map[string][]string) []map[string]string {
	if len(ids) == 0 {
		return []map[string]string{{}}
	}
	head := ids[0]
	rest := cartesian(ids[1:], secrets)
	var result []map[string]string
	for _, val := range secrets[head] {
		for _, m := range rest {
			combo := make(map[string]string, len(m)+1)
			maps.Copy(combo, m)
			combo[head] = val
			result = append(result, combo)
		}
	}
	return result
}
