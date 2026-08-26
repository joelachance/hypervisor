// Package text holds the small string helpers hv renders with.
//
// They were duplicated in three packages with two different meanings of
// "plural", which is how "2 branchess" reached a terminal.
package text

import "strings"

// Plural returns the suffix for a count: "s" for none or many, "" for one.
// Callers pass the suffix their word needs - "s", "es".
func Plural(suffix string, count int) string {
	if count == 1 {
		return ""
	}
	return suffix
}

// Truncate caps a string to a rune width, marking what it cut.
func Truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit < 2 {
		return string(runes[:limit])
	}
	return string(runes[:limit-1]) + "…"
}

// Short is a shortened object id, for output where the full one is noise.
func Short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// FirstNonEmpty is the first value with something in it.
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
