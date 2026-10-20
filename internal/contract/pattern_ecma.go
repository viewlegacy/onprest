package contract

import (
	"fmt"
	"strings"
)

// ECMAExpression is generated from the same normalized scalar AST as the Go
// expression. It requires ECMA-262 Unicode mode. It never translates Go escapes.
func (p *Pattern) ECMAExpression() string { return ecmaNode(p.AST) }
func ecmaRune(r rune) string              { return fmt.Sprintf(`\u{%x}`, r) }
func ecmaNode(n *PatternNode) string {
	switch n.Kind {
	case "NoMatch":
		return "(?!)"
	case "EmptyMatch":
		return "(?:)"
	case "Literal":
		var b strings.Builder
		for _, r := range n.Text {
			b.WriteString(ecmaRune(r))
		}
		return b.String()
	case "CharClass":
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < len(n.Ranges); i += 2 {
			b.WriteString(ecmaRune(n.Ranges[i]))
			if n.Ranges[i] != n.Ranges[i+1] {
				b.WriteByte('-')
				b.WriteString(ecmaRune(n.Ranges[i+1]))
			}
		}
		b.WriteByte(']')
		return b.String()
	case "BeginText":
		return "^"
	case "EndText":
		return "(?![\\s\\S])" // Unlike $, this rejects a final newline.
	case "Concat":
		var b strings.Builder
		for _, c := range n.Children {
			b.WriteString(ecmaNode(c))
		}
		return b.String()
	case "Alternate":
		a := []string{}
		for _, c := range n.Children {
			a = append(a, ecmaNode(c))
		}
		return "(?:" + strings.Join(a, "|") + ")"
	case "Star", "Plus", "Quest", "Repeat":
		suffix := map[string]string{"Star": "*", "Plus": "+", "Quest": "?"}[n.Kind]
		if n.Kind == "Repeat" {
			if n.Max < 0 {
				suffix = fmt.Sprintf("{%d,}", n.Min)
			} else if n.Min == n.Max {
				suffix = fmt.Sprintf("{%d}", n.Min)
			} else {
				suffix = fmt.Sprintf("{%d,%d}", n.Min, n.Max)
			}
		}
		if n.Lazy {
			suffix += "?"
		}
		return "(?:" + ecmaNode(n.Children[0]) + ")" + suffix
	default:
		panic("unsupported normalized common-pattern node")
	}
}
