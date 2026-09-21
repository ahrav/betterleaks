package internal

// CompiledRegexp is an interface satisfied by both *stdlib.Regexp and *github.com/betterleaks/go-re2.Regexp.
type CompiledRegexp interface {
	MatchString(s string) bool
	FindString(s string) string
	FindStringSubmatch(s string) []string
	FindAllStringIndex(s string, n int) [][]int
	ReplaceAllString(src, repl string) string
	NumSubexp() int
	SubexpNames() []string
	String() string
}

// AnchoredFinder is implemented by engines that can try a match at each of a
// set of candidate start offsets while keeping the whole input as context.
type AnchoredFinder interface {
	FindAllStringIndexAt(s string, starts []int, n int) [][]int
}
