package contract

import (
	"strconv"
	"strings"
)

// integralJSONNumber decides integrality of JSON number text without
// expanding its exponent, so input like 1e-999999 costs time linear in its
// length instead of building a huge rational. digits has no leading or
// trailing zeros ("" for zero) and the value is ±digits×10^zeros.
func integralJSONNumber(text string) (negative bool, digits string, zeros int64, ok bool) {
	s := text
	if strings.HasPrefix(s, "-") {
		negative, s = true, s[1:]
	}
	mantissa, exponent, hasExponent := s, "", false
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent, hasExponent = s[:i], s[i+1:], true
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	if whole == "" || !decimalDigits(whole) || (strings.Contains(mantissa, ".") && (fraction == "" || !decimalDigits(fraction))) {
		return false, "", 0, false
	}
	var exp int64
	if hasExponent {
		sign := int64(1)
		if strings.HasPrefix(exponent, "+") || strings.HasPrefix(exponent, "-") {
			if exponent[0] == '-' {
				sign = -1
			}
			exponent = exponent[1:]
		}
		if exponent == "" || !decimalDigits(exponent) {
			return false, "", 0, false
		}
		for _, c := range exponent {
			// Saturate far beyond any digit count a bounded body can hold.
			if exp < 1<<40 {
				exp = exp*10 + int64(c-'0')
			}
		}
		exp *= sign
	}
	significand := strings.TrimLeft(whole+fraction, "0")
	if significand == "" {
		return negative, "", 0, true
	}
	exp -= int64(len(fraction))
	trimmed := strings.TrimRight(significand, "0")
	exp += int64(len(significand) - len(trimmed))
	if exp < 0 {
		return false, "", 0, false
	}
	return negative, trimmed, exp, true
}
func decimalDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// JSONNumberIsInteger reports whether JSON number text denotes an integer.
func JSONNumberIsInteger(text string) bool {
	_, _, _, ok := integralJSONNumber(text)
	return ok
}

// JSONNumberInt64 returns the exact int64 value of integral JSON number text.
func JSONNumberInt64(text string) (int64, bool) {
	negative, digits, zeros, ok := integralJSONNumber(text)
	if !ok {
		return 0, false
	}
	if digits == "" {
		return 0, true
	}
	if int64(len(digits))+zeros > 19 {
		return 0, false
	}
	s := digits + strings.Repeat("0", int(zeros))
	if negative {
		s = "-" + s
	}
	n, e := strconv.ParseInt(s, 10, 64)
	return n, e == nil
}
