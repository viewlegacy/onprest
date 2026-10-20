// Package sqlsyntax contains the shared, driver-aware SQL lexical contract.
package sqlsyntax

import "strings"

type Operation string

const (
	Select Operation = "select"
	Insert Operation = "insert"
	Update Operation = "update"
	Delete Operation = "delete"
)

func (o Operation) Mutation() bool { return o != "" && o != Select }

type Classification struct {
	Keyword     string
	Operation   Operation
	Single      bool
	ReturnsRows bool
}

func isIdent(b byte) bool { return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ParameterName reads a named SQL parameter without consuming SQL punctuation.
func ParameterName(s string) (string, int) {
	if len(s) == 0 || !isIdent(s[0]) {
		return "", 0
	}
	i := 1
	for i < len(s) && (isIdent(s[i]) || isDigit(s[i])) {
		i++
	}
	return s[:i], i
}

// Parameters returns named bindings outside protected regions, in occurrence order.
func Parameters(driver, query string) []string {
	var names []string
	for i := 0; i < len(query); {
		if n, ok := ProtectedRegionEnd(driver, query, i); ok {
			i = n
			continue
		}
		if query[i] == ':' {
			if i+1 < len(query) && query[i+1] == ':' {
				i += 2
				continue
			}
			name, n := ParameterName(query[i+1:])
			if n > 0 {
				names = append(names, name)
				i += n + 1
				continue
			}
		}
		i++
	}
	return names
}
func Classify(driver, query string) Classification {
	tokens, single := scanSQLTokens(driver, query)
	c := Classification{Single: single}
	if len(tokens) == 0 {
		return c
	}
	c.Keyword = tokens[0]
	switch c.Keyword {
	case "select":
		c.Operation = Select
	case "insert":
		c.Operation = Insert
	case "update":
		c.Operation = Update
	case "delete":
		c.Operation = Delete
	}
	if c.Operation.Mutation() {
		for _, token := range tokens[1:] {
			if token == "returning" || (driver == "sqlserver" && token == "output") {
				c.ReturnsRows = true
				break
			}
		}
	}
	return c
}

// scanSQLTokens returns top-level lexical tokens outside quoted/comment regions.
// It intentionally does not parse SQL grammar: the first token and row-returning
// DML clauses are the only syntax facts required by the public contract.
func scanSQLTokens(driver, query string) ([]string, bool) {
	tokens := []string{}
	single := true
	for i := 0; i < len(query); {
		if next, ok := ProtectedRegionEnd(driver, query, i); ok {
			i = next
			continue
		}
		switch {
		case query[i] == ';':
			if !onlySQLTrivia(driver, query[i+1:]) {
				single = false
			}
			i++
		case isIdent(query[i]):
			start := i
			for i < len(query) && (isIdent(query[i]) || isDigit(query[i])) {
				i++
			}
			tokens = append(tokens, strings.ToLower(query[start:i]))
		default:
			i++
		}
	}
	return tokens, single
}

func hasSingleSQLStatement(driver, query string) bool {
	for i := 0; i < len(query); {
		if next, ok := ProtectedRegionEnd(driver, query, i); ok {
			i = next
			continue
		}
		switch {
		case query[i] == ';':
			return onlySQLTrivia(driver, query[i+1:])
		default:
			i++
		}
	}
	return true
}

// protectedSQLRegionEnd is the single dialect-aware lexical boundary used by
// classification, single-statement checks, and placeholder replacement.
func ProtectedRegionEnd(driver, query string, start int) (int, bool) {
	switch {
	case query[start] == '\'':
		return skipSQLQuoted(query, start, '\'', driver == "postgres" && isPostgresEscapeStringPrefix(query, start)), true
	case query[start] == '"':
		return skipSQLQuoted(query, start, '"', false), true
	case driver == "mysql" && query[start] == '`':
		return skipSQLQuoted(query, start, '`', false), true
	case driver == "sqlserver" && query[start] == '[':
		return skipSQLBracketIdentifier(query, start), true
	case driver == "oracle" && isOracleAlternativeQuotePrefix(query, start):
		return skipOracleAlternativeQuote(query, start), true
	case start+1 < len(query) && query[start:start+2] == "--":
		return skipSQLLineComment(query, start), true
	case start+1 < len(query) && query[start:start+2] == "/*":
		return skipSQLBlockComment(driver, query, start), true
	case driver == "postgres" && query[start] == '$':
		return skipSQLDollarQuote(query, start)
	default:
		return 0, false
	}
}

func skipSQLQuoted(query string, start int, quote byte, backslashEscapes bool) int {
	for i := start + 1; i < len(query); i++ {
		if backslashEscapes && query[i] == '\\' && i+1 < len(query) {
			i++
			continue
		}
		if query[i] != quote {
			continue
		}
		if i+1 < len(query) && query[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}

func isPostgresEscapeStringPrefix(query string, quote int) bool {
	if quote == 0 || (query[quote-1] != 'e' && query[quote-1] != 'E') {
		return false
	}
	return quote == 1 || !isIdent(query[quote-2])
}

func skipSQLBracketIdentifier(query string, start int) int {
	for i := start + 1; i < len(query); i++ {
		if query[i] != ']' {
			continue
		}
		if i+1 < len(query) && query[i+1] == ']' {
			i++
			continue
		}
		return i + 1
	}
	return len(query)
}

func isOracleAlternativeQuotePrefix(query string, quote int) bool {
	if quote+2 >= len(query) || (query[quote] != 'q' && query[quote] != 'Q') || query[quote+1] != '\'' {
		return false
	}
	return quote == 0 || !isIdent(query[quote-1])
}

func skipOracleAlternativeQuote(query string, start int) int {
	opening := query[start+2]
	closing := opening
	switch opening {
	case '[':
		closing = ']'
	case '(':
		closing = ')'
	case '{':
		closing = '}'
	case '<':
		closing = '>'
	}
	for i := start + 3; i+1 < len(query); i++ {
		if query[i] == closing && query[i+1] == '\'' {
			return i + 2
		}
	}
	return len(query)
}

func skipSQLLineComment(query string, start int) int {
	if end := strings.IndexByte(query[start+2:], '\n'); end >= 0 {
		return start + 2 + end + 1
	}
	return len(query)
}

func skipSQLBlockComment(driver, query string, start int) int {
	// PostgreSQL and SQL Server accept nested block comments. Treating the
	// first */ as the end would expose text that is still comment trivia to
	// the classifier and can turn a writable statement into an apparent
	// SELECT. MySQL and Oracle retain their non-nesting lexical behavior.
	depth := 1
	for i := start + 2; i+1 < len(query); i++ {
		pair := query[i : i+2]
		if pair == "/*" && (driver == "postgres" || driver == "sqlserver") {
			depth++
			i++
			continue
		}
		if pair == "*/" {
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(query)
}

func skipSQLDollarQuote(query string, start int) (int, bool) {
	endTag := start + 1
	for endTag < len(query) && (isIdent(query[endTag]) || isDigit(query[endTag])) {
		endTag++
	}
	if endTag >= len(query) || query[endTag] != '$' {
		return start, false
	}
	tag := query[start : endTag+1]
	end := strings.Index(query[endTag+1:], tag)
	if end < 0 {
		return len(query), true
	}
	return endTag + 1 + end + len(tag), true
}

func onlySQLTrivia(driver, query string) bool {
	for i := 0; i < len(query); {
		switch {
		case query[i] == ' ' || query[i] == '\t' || query[i] == '\r' || query[i] == '\n':
			i++
		case i+1 < len(query) && query[i:i+2] == "--":
			i = skipSQLLineComment(query, i)
		case i+1 < len(query) && query[i:i+2] == "/*":
			i = skipSQLBlockComment(driver, query, i)
		default:
			return false
		}
	}
	return true
}

func firstSQLKeyword(query string) string {
	s := strings.TrimSpace(query)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			i := skipSQLBlockComment("postgres", s, 0)
			if i < len(s) {
				s = strings.TrimSpace(s[i:])
				continue
			}
			return ""
		}
		break
	}
	for i, r := range s {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				return strings.ToLower(s[:i])
			}
		}
	}
	return strings.ToLower(s)
}
