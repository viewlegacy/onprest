package contract

import "testing"

func TestFilesystemSelectorRegexpCaseFlagsAndLiteralSyntax(t *testing.T) {
	for _, pattern := range []string{`(?i)b`, `(?i:b)`, `(?im-s:b)`, `x(?i)b`, `(?i-i)b`, `(?i:123)`, `[[:alpha:]](?i)b`, `\Qliteral\E(?i)b`} {
		if r, e := CompileSelectorRegexp(pattern); r != nil || e == nil {
			t.Fatal("casefold flag accepted", pattern)
		}
	}
	for _, tc := range []struct{ pattern, input string }{{`\(\?i\)`, `(?i)`}, {`\Q(?i)\E`, `(?i)`}, {`\Qabc`, `abc`}, {`\Q(?i)abc`, `(?i)abc`}, {`\Qabc\`, `abc\`}, {`\Q`, ``}, {`[(?i)]+`, `(?i)`}, {`[[:alpha:](?i)]+`, `(?i)`}, {`[](?i)]+`, `(?i)`}, {`[Aa]`, `A`}, {`a|A`, `A`}, {`(?-i)b`, `b`}, {`(?m)b`, `b`}, {`(?P<i>b)`, `b`}, {`(?<i>b)`, `b`}} {
		r, e := CompileSelectorRegexp(tc.pattern)
		if e != nil {
			t.Fatalf("literal/explicit class rejected %q: %v", tc.pattern, e)
		}
		if !r.MatchString(tc.input) || r.MatchString("/"+tc.input) || r.MatchString(tc.input+"/") {
			t.Fatalf("whole-name matching %q", tc.pattern)
		}
	}
}
