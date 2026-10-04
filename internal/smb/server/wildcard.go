package server

import (
	"strings"
	"unicode/utf16"
)

// matchPattern follows MS-FSA 2.1.4.4 with case-sensitive comparison.
// An empty search pattern, like "*", selects every directory entry.
func matchPattern(pattern, name string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if name == "" {
		return false
	}
	if pattern == "*.*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?<>\"") {
		return pattern == name
	}

	// MS-FSA compares WCHARs, so a supplementary character occupies two units.
	expression := utf16.Encode([]rune(pattern))
	filename := utf16.Encode([]rune(name))
	lastDot := -1
	for i, char := range filename {
		if char == '.' {
			lastDot = i
		}
	}

	// Each row records matches between one expression suffix and every name
	// suffix. Two rows avoid recursive backtracking and use linear space.
	next := make([]bool, len(filename)+1)
	current := make([]bool, len(next))
	next[len(filename)] = true
	for i := len(expression) - 1; i >= 0; i-- {
		for j := len(filename); j >= 0; j-- {
			end := j == len(filename)
			switch expression[i] {
			case '*':
				current[j] = next[j] || (!end && current[j+1])
			case '?':
				current[j] = !end && next[j+1]
			case '<': // DOS_STAR cannot consume the final dot.
				current[j] = next[j] || (!end && j != lastDot && current[j+1])
			case '>': // Contiguous DOS_QMs skip together at a dot or end.
				if end || filename[j] == '.' {
					current[j] = next[j]
				} else {
					current[j] = next[j+1]
				}
			case '"': // DOS_DOT consumes a dot, or nothing at end.
				current[j] = (end && next[j]) || (!end && filename[j] == '.' && next[j+1])
			default:
				current[j] = !end && expression[i] == filename[j] && next[j+1]
			}
		}
		next, current = current, next
	}
	return next[0]
}
