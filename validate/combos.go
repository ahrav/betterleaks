package validate

import (
	"sort"

	"github.com/betterleaks/betterleaks/logging"
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
			for k, v := range m {
				combo[k] = v
			}
			combo[head] = val
			result = append(result, combo)
		}
	}
	return result
}
