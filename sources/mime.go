package sources

import (
	"sync"

	"github.com/h2non/filetype"
	"github.com/h2non/filetype/types"
)

// matchType reports the file type filetype.Match would return for buf.
//
// filetype.Match looks every matcher up in a map keyed by types.Type on every
// call, so classifying one file hashes a hundred struct keys. This walks a
// snapshot of the same matchers in the same order, so the first match, and
// therefore the result, is unchanged.
func matchType(buf []byte) (types.Type, error) {
	if len(buf) == 0 {
		return types.Unknown, filetype.ErrEmptyBuffer
	}
	for _, m := range orderedMatchers() {
		if m.match(buf) {
			return m.kind, nil
		}
	}
	return types.Unknown, nil
}

type typeMatcher struct {
	kind  types.Type
	match func([]byte) bool
}

var (
	matcherOnce sync.Once
	matchers    []typeMatcher
)

func orderedMatchers() []typeMatcher {
	matcherOnce.Do(func() {
		for _, kind := range *filetype.MatcherKeys {
			checker := filetype.Matchers[kind]
			if checker == nil || kind.Extension == "" {
				continue
			}
			matchers = append(matchers, typeMatcher{kind: kind, match: func(buf []byte) bool {
				return checker(buf) != types.Unknown
			}})
		}
	})
	return matchers
}
