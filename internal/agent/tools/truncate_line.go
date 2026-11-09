package tools

import "strings"

// lineTruncatedSuffix marks a line that view or grep cut short.
const lineTruncatedSuffix = "... [truncated]"

// truncateLine cuts line to at most maxBytes bytes, at a rune boundary so
// multi-byte characters are never split, and appends lineTruncatedSuffix.
// Lines within the limit are returned unchanged.
func truncateLine(line string, maxBytes int) string {
	if len(line) <= maxBytes {
		return line
	}
	return strings.ToValidUTF8(line[:maxBytes], "") + lineTruncatedSuffix
}
