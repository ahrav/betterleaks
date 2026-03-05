package detect

// extractContext extracts bytes before and after the match from the fragment raw content.
// matchIndex is the [start, end) byte range of the match within raw.
func extractContext(raw string, matchIndex []int, matchContextBytes int) string {
	if matchContextBytes <= 0 || len(raw) == 0 {
		return ""
	}

	start := max(matchIndex[0]-matchContextBytes, 0)
	end := min(matchIndex[1]+matchContextBytes, len(raw))

	return raw[start:end]
}
