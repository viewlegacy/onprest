package contract

import "testing"

func TestCommonPatternASTAndAgentSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		yes, no []string
	}{
		{"", []string{"", "x", "\n"}, nil}, {"a", []string{"a", "cat"}, []string{"", "b"}},
		{"^a$", []string{"a"}, []string{"ba", "ab", "a\n", "a\r", "a\u2028", "a\u2029"}},
		{"^.$", []string{"x", "é", "😀"}, []string{"", "xx", "\n", "\r", "\u2028", "\u2029"}},
		{`^\d+$`, []string{"0123456789"}, []string{"１２", "١", "A"}}, {`^\D$`, []string{"é", "😀", "\n"}, []string{"0"}},
		{`^\w+$`, []string{"Az_09"}, []string{"é", "😀", "a-"}}, {`^\W$`, []string{"é", "😀", "\n"}, []string{"_", "A", "0"}},
		{`^[^a-z]$`, []string{"😀", "A", "\n"}, []string{"a", "z"}}, {`^(?:猫|😀){1,2}?$`, []string{"猫", "😀猫"}, []string{"", "猫猫猫"}},
		{`^\x41[\x42-\x43]$`, []string{"AB", "AC"}, []string{"AD"}}, {`^a\/b$`, []string{"a/b"}, []string{"a\\b"}},
		{`^\\/$`, []string{"\\/"}, []string{"/"}}, {`^[\-a]+$`, []string{"-aa"}, []string{"b"}},
		{`^\n\r\t\f\v$`, []string{"\n\r\t\f\v"}, []string{"nrtfv"}},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			p, e := CompilePattern(c.pattern)
			if e != nil {
				t.Fatal(e)
			}
			if p.AST == nil {
				t.Fatal("AST missing")
			}
			for _, v := range c.yes {
				if !p.MatchString(v) {
					t.Fatalf("%q should match %s", v, p.GoExpression)
				}
			}
			for _, v := range c.no {
				if p.MatchString(v) {
					t.Fatalf("%q must not match", v)
				}
			}
		})
	}
}
func TestCommonPatternRejectsOutsideSubset(t *testing.T) {
	for _, s := range []string{`(?i)a`, `(?m)^a`, `(?=a)`, `(?!a)`, `(?<=a)`, `(?P<name>a)`, `(?<name>a)`, `(a)\1`, `\p{L}`, `[[:alpha:]]`, `\s`, `\b`, `\u0041`, `\x{41}`, `\a`, `\Qx\E`, `a{1001}`, `a{2,1}`, `[a`, `a{`, `\-`, string([]byte{0xff})} {
		t.Run(s, func(t *testing.T) {
			if _, e := CompilePattern(s); e == nil {
				t.Fatal("unsupported syntax accepted")
			}
		})
	}
}
func TestPatternEquivalentCaptureAndASCIINormalization(t *testing.T) {
	a, e := CompilePattern(`^(a|b)\d$`)
	if e != nil {
		t.Fatal(e)
	}
	b, e := CompilePattern(`^(?:a|b)[0-9]$`)
	if e != nil {
		t.Fatal(e)
	}
	if string(canonical(t, a.AST.normalized())) != string(canonical(t, b.AST.normalized())) {
		t.Fatal("same pattern AST differs")
	}
}
