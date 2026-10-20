package contract

import (
	"errors"
	"regexp"
	"strings"
)

// CompileSelectorRegexp keeps Go regexp syntax and binds the complete basename.
// Selector matching is case-sensitive: enabling the i flag is rejected even
// when later disabled or applied only to digits. Do not detect this via the
// optimized regexp AST: Go rewrites explicit [Aa] and a|A into FoldCase literals.
// Validate syntax first, then inspect flag groups while skipping escaped/quoted
// literals and character classes, including their POSIX sub-classes.
func CompileSelectorRegexp(pattern string) (*regexp.Regexp, error) {
	if len(pattern) < 1 || len(pattern) > 4096 {
		return nil, errors.New("selector regex must be 1..4096 bytes")
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return nil, errors.New("invalid selector Go regexp")
	}
	terminalQuote := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			if i+1 < len(pattern) && pattern[i+1] == 'Q' {
				end := strings.Index(pattern[i+2:], `\E`)
				if end < 0 {
					terminalQuote = true
					i = len(pattern)
				} else {
					i += end + 3
				}
			} else {
				i++
			}
		case '[':
			i++
			if i < len(pattern) && pattern[i] == '^' {
				i++
			}
			if i < len(pattern) && pattern[i] == ']' {
				i++
			}
			for ; i < len(pattern) && pattern[i] != ']'; i++ {
				if pattern[i] == '\\' {
					i++
					continue
				}
				if pattern[i] == '[' && i+1 < len(pattern) && pattern[i+1] == ':' {
					end := strings.Index(pattern[i+2:], ":]")
					if end >= 0 {
						i += end + 3
					}
				}
			}
		case '(':
			if i+1 >= len(pattern) || pattern[i+1] != '?' {
				continue
			}
			enable := true
			for j := i + 2; j < len(pattern); j++ {
				flag := pattern[j]
				if flag == '-' {
					enable = false
					continue
				}
				if flag != 'i' && flag != 'm' && flag != 's' && flag != 'U' {
					break
				}
				if flag == 'i' && enable {
					return nil, errors.New("selector regex must be case-sensitive")
				}
			}
		}
	}
	if terminalQuote {
		// Go accepts a quoted literal through end-of-pattern without \E.
		// Close that quote before appending our group/anchor, preserving the
		// original literal instead of quoting the generated wrapper as well.
		pattern += `\E`
	}
	return regexp.Compile(`\A(?:` + pattern + `)\z`)
}
