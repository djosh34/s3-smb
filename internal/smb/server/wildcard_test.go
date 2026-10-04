package server

import (
	"strings"
	"testing"
)

func TestDirectoryMatchingTreatsLiteralCharactersAndDOSWildcardsCorrectly(t *testing.T) {
	for _, test := range []struct {
		description string
		pattern     string
		name        string
		want        bool
	}{
		{"empty pattern", "", "report.txt", true},
		{"empty pattern and name", "", "", true},
		{"star", "*", "report.txt", true},
		{"star and empty name", "*", "", true},
		{"empty name", "report", "", false},
		{"empty name with DOS wildcard", ">", "", false},
		{"empty name with DOS dot", "\"", "", false},
		{"empty name with multiple stars", "**", "", false},
		{"empty name with star dot star", "*.*", "", false},
		{"brackets regression 105", "part[1]*", "part[1].txt", true},
		{"brackets are not a character class", "part[1]*", "part1.txt", false},
		{"parentheses regression 105", "part(1)*", "part(1).txt", true},
		{"parentheses are not a group", "part(1)*", "part1.txt", false},
		{"dot regression 105", "*.txt", "reportXtxt", false},
		{"dot is literal", "*.txt", "report.txt", true},
		{"dot required", "*.txt", "reporttxt", false},
		{"literal regex characters", "a[1](b){2}+^$|\\.*", "a[1](b){2}+^$|\\.txt", true},
		{"regex alternation is literal", "a|b*", "b.txt", false},
		{"backslash is not an escape", "a\\?", "a\\b", true},
		{"backslash required", "a\\?", "ab", false},
		{"exact name", "report.txt", "report.txt", true},
		{"exact mismatch", "report.txt", "reportXtxt", false},
		{"case sensitive literal", "Report.txt", "report.txt", false},
		{"case sensitive wildcard", "R*", "report.txt", false},
		{"case sensitive extension", "*.TXT", "report.txt", false},
		{"no Unicode normalization", "é*", "e\u0301.txt", false},
		{"star dot star without dot", "*.*", "report", true},
		{"star dot star trailing dot", "*.*", "report.", true},
		{"star dot star multiple dots", "*.*", "report.old.txt", true},
		{"star matches zero", "re*port", "report", true},
		{"star matches many", "re*port", "read-report", true},
		{"star matches dots", "re*txt", "report.old.txt", true},
		{"anchored prefix", "report*", "old-report.txt", false},
		{"anchored suffix", "*.txt", "report.txt.old", false},
		{"multiple stars", "**re***port**.txt**", "report.txt", true},
		{"multiple stars with intervening text", "*a*b*c*", "0a1b2c3", true},
		{"multiple stars mismatch", "*a*b*c*", "0a1c2b3", false},
		{"question matches one", "a?c", "abc", true},
		{"question matches a dot", "a?c", "a.c", true},
		{"question needs a character", "a?", "a", false},
		{"question cannot match two", "a?", "abc", false},
		{"question matches UTF16 unit", "?", "é", true},
		{"question cannot match surrogate pair", "?", "😀", false},
		{"two questions match surrogate pair", "??", "😀", true},
		{"Unicode literal with star", "😀*", "😀.txt", true},
		{"DOS star without dot", "<", "report", true},
		{"DOS star stops at final dot", "<", "report.txt", false},
		{"DOS star leaves extension", "<.txt", "report.txt", true},
		{"DOS star consumes earlier dots", "<.txt", "report.old.txt", true},
		{"DOS star cannot cross final dot", "<txt", "report.txt", false},
		{"DOS star can match zero", "a<.txt", "a.txt", true},
		{"DOS star with trailing dot", "<.", "report.", true},
		{"DOS star alone rejects trailing dot", "<", "report.", false},
		{"DOS star with multiple trailing dots", "<.", "report...", true},
		{"multiple DOS stars", "<<<.txt", "report.old.txt", true},
		{"DOS question matches one", "a>c", "abc", true},
		{"DOS question at end", "a>", "a", true},
		{"DOS questions skip at end", "a>>>", "a", true},
		{"DOS questions skip at dot", "a>>>.txt", "a.txt", true},
		{"DOS questions consume then skip", "a>>>.txt", "ab.txt", true},
		{"DOS questions consume all", "a>>>.txt", "abcd.txt", true},
		{"DOS questions cannot consume too many", "a>>>.txt", "abcde.txt", false},
		{"DOS question does not consume dot", "a>", "a.", false},
		{"DOS question skip does not skip literals", "a>b>", "a", false},
		{"DOS question uses UTF16 units", ">", "😀", false},
		{"DOS questions match surrogate pair", ">>", "😀", true},
		{"DOS dot consumes dot", "a\"txt", "a.txt", true},
		{"DOS dot at end", "a\"", "a", true},
		{"DOS dot matches trailing dot", "a\"", "a.", true},
		{"DOS dot cannot disappear inside name", "a\"txt", "atxt", false},
		{"DOS dot cannot consume ordinary character", "a\"txt", "aXtxt", false},
		{"multiple DOS dots at end", "a\"\"", "a", true},
		{"multiple DOS dots consume trailing dots", "a\"\"", "a..", true},
		{"DOS star and dot without extension", "<\"", "report", true},
		{"DOS star and dot trailing dot", "<\"", "report.", true},
		{"DOS star and dot reject extension", "<\"", "report.txt", false},
		{"DOS wildcard combination", "<\">>>", "report.txt", true},
		{"DOS wildcard combination without dot", "<\">>>", "report", true},
		{"DOS wildcard combination trailing dot", "<\">>>", "report.", true},
		{"DOS wildcard combination long extension", "<\">>>", "report.text", false},
	} {
		t.Run(test.description, func(t *testing.T) {
			if got := matchPattern(test.pattern, test.name); got != test.want {
				t.Fatalf("matchPattern(%q, %q) = %t, want %t", test.pattern, test.name, got, test.want)
			}
		})
	}
}

func FuzzMatchPattern(f *testing.F) {
	f.Add("", "")
	f.Add("part[1]*", "part[1].txt")
	f.Add("part(1)*", "part(1).txt")
	f.Add("*.txt", "reportXtxt")
	f.Add("<\">>>", "report.txt")
	f.Add("a>>>.txt", "a.txt")
	f.Add("<.", "report...")
	f.Add("**a***b**", "aaaaab")
	f.Add("??", "😀")
	f.Add("a[1](b){2}+^$|\\.", "a[1](b){2}+^$|\\.")
	f.Add("\xff", "\xff")
	f.Fuzz(func(t *testing.T, pattern, name string) {
		// Every input must finish without a panic. Literal names must match
		// themselves, including arbitrary byte strings from the fuzzer.
		matchPattern(pattern, name)
		if !strings.ContainsAny(name, "*?<>\"") && !matchPattern(name, name) {
			t.Fatalf("literal name %q did not match itself", name)
		}
	})
}
