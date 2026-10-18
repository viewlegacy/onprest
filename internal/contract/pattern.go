package contract

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CompilePattern checks original syntax before using the Go parser. This avoids
// accepting Go-only syntax merely because it compiles.
func CompilePattern(source string) (*Pattern, error) {
	if !utf8.ValidString(source) {
		return nil, errors.New("pattern must contain Unicode scalars")
	}
	inClass := false
	// ECMA-262 forbids quantifying an assertion (^* or $+) while Go accepts
	// it, so a quantifier directly after an anchor is outside the subset.
	afterAnchor := false
	for i := 0; i < len(source); i++ {
		c := source[i]
		anchor := afterAnchor
		afterAnchor = false
		if !inClass && anchor && (c == '*' || c == '+' || c == '?' || c == '{') {
			return nil, errors.New("quantifier after an anchor is unsupported")
		}
		if c == '\\' {
			i++
			if i >= len(source) {
				return nil, errors.New("incomplete pattern escape")
			}
			c = source[i]
			switch c {
			case '\\', '/', 'n', 'r', 't', 'f', 'v', 'd', 'D', 'w', 'W':
			case '.', '*', '+', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|':
			case '-':
				if !inClass {
					return nil, errors.New("hyphen escape is class-only")
				}
			case 'x':
				if i+2 >= len(source) {
					return nil, errors.New("invalid hex escape")
				}
				if _, e := strconv.ParseUint(source[i+1:i+3], 16, 8); e != nil {
					return nil, errors.New("invalid hex escape")
				}
				i += 2
			default:
				return nil, errors.New("unsupported pattern escape")
			}
			continue
		}
		if inClass {
			if c == '[' {
				return nil, errors.New("nested or POSIX class is unsupported")
			}
			if c == ']' {
				inClass = false
			}
			continue
		}
		switch c {
		case '[':
			// "[]" and "[^]" are an empty and an any-character class in
			// ECMA-262 but a literal ']' in Go, so a leading ']' is refused.
			first := i + 1
			if first < len(source) && source[first] == '^' {
				first++
			}
			if first < len(source) && source[first] == ']' {
				return nil, errors.New("class starting with ']' is unsupported")
			}
			inClass = true
		case '^', '$':
			afterAnchor = true
		case ']', '}':
			return nil, errors.New("unescaped pattern syntax character")
		case '(':
			if i+1 < len(source) && source[i+1] == '?' {
				if i+2 >= len(source) || source[i+2] != ':' {
					return nil, errors.New("unsupported pattern group")
				}
				i += 2
			}
		case '{':
			end := strings.IndexByte(source[i+1:], '}')
			if end < 0 {
				return nil, errors.New("invalid repetition")
			}
			body := source[i+1 : i+1+end]
			parts := strings.Split(body, ",")
			if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
				return nil, errors.New("invalid repetition")
			}
			for j, p := range parts {
				if j == 1 && p == "" {
					continue
				}
				for _, r := range p {
					if r < '0' || r > '9' {
						return nil, errors.New("invalid repetition")
					}
				}
			}
			i += end + 1
		}
	}
	if inClass {
		return nil, errors.New("unterminated pattern class")
	}
	// Go's parser does not accept the ECMA identity escape for slash.
	var escaped strings.Builder
	for i := 0; i < len(source); i++ {
		if source[i] == '\\' && i+1 < len(source) {
			i++
			if source[i] != '/' {
				escaped.WriteByte('\\')
			}
			escaped.WriteByte(source[i])
			continue
		}
		escaped.WriteByte(source[i])
	}
	source = escaped.String()
	tree, e := syntax.Parse(source, syntax.Perl)
	if e != nil {
		return nil, errors.New("invalid common pattern")
	}
	tree = normalizePattern(tree)
	expr := tree.String()
	re, e := regexp.Compile(expr)
	if e != nil {
		return nil, errors.New("invalid generated pattern")
	}
	return &Pattern{AST: patternNode(tree), GoExpression: expr, re: re}, nil
}
func normalizePattern(r *syntax.Regexp) *syntax.Regexp {
	for i, s := range r.Sub {
		r.Sub[i] = normalizePattern(s)
	}
	if r.Op == syntax.OpCapture {
		return r.Sub[0]
	}
	if r.Op == syntax.OpAnyCharNotNL || r.Op == syntax.OpAnyChar {
		r = &syntax.Regexp{Op: syntax.OpCharClass, Rune: []rune{0, 9, 11, 12, 14, 0x2027, 0x202a, utf8.MaxRune}}
	}
	return r
}
func patternNode(r *syntax.Regexp) *PatternNode {
	n := &PatternNode{Kind: r.Op.String(), Min: r.Min, Max: r.Max, Lazy: r.Flags&syntax.NonGreedy != 0}
	if r.Op == syntax.OpLiteral {
		n.Text = string(r.Rune)
	} else if r.Op == syntax.OpCharClass {
		n.Ranges = append([]rune{}, r.Rune...)
	}
	for _, s := range r.Sub {
		n.Children = append(n.Children, patternNode(s))
	}
	return n
}
func (n *PatternNode) normalized() map[string]any {
	m := map[string]any{"kind": n.Kind}
	if n.Kind == "Literal" {
		m["text"] = n.Text
	}
	if len(n.Ranges) > 0 {
		r := []any{}
		for _, v := range n.Ranges {
			r = append(r, int64(v))
		}
		m["ranges"] = r
	}
	if len(n.Children) > 0 {
		c := []any{}
		for _, s := range n.Children {
			c = append(c, s.normalized())
		}
		m["children"] = c
	}
	if n.Kind == "Repeat" {
		m["min"] = int64(n.Min)
		m["max"] = int64(n.Max)
	}
	if n.Lazy {
		m["lazy"] = true
	}
	return m
}
