package harnesses

import "strings"

// ClientName is an MCP client name as the MCP Events client table compares
// it (internal/mcpevents/event): spaces and tabs trimmed, ASCII letters
// lowercased, anything else left as it is. ASCII only, so every SDK
// (Go, TypeScript, Python) normalizes a name the same way: Unicode case
// folding and whitespace differ between languages (Turkish İ, NEL, BOM),
// and a name with such a character simply matches no ASCII entry.
func ClientName(s string) string {
	return ASCIILower(strings.Trim(s, " \t"))
}

// ASCIILower lowercases ASCII letters only.
func ASCIILower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
