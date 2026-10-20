package contract

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func TestJSONIntegerDecisionMatchesExactRationals(t *testing.T) {
	for _, text := range []string{"0", "-0", "0.0", "0e5", "1", "-1", "10", "1.0", "1.5", "1e2", "1E+2", "1e-2", "100e-2", "123e-1", "1230e-1", "0.000e999", "9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809", "9.223372036854775807e18", "922337203685477580.7e1", "1e18", "1e19", "12e17", "-0.5e1", "5e-1"} {
		r, valid := new(big.Rat).SetString(text)
		if !valid || !validJSONNumberText(text) {
			t.Fatalf("fixture %s is not a JSON number", text)
		}
		wantInteger := r.IsInt()
		if got := JSONNumberIsInteger(text); got != wantInteger {
			t.Fatalf("%s integer=%v want %v", text, got, wantInteger)
		}
		n, ok := JSONNumberInt64(text)
		wantInt64 := wantInteger && r.Num().IsInt64()
		if ok != wantInt64 || ok && n != r.Num().Int64() {
			t.Fatalf("%s int64=%d,%v want %v", text, n, ok, wantInt64)
		}
	}
}

// validJSONNumberText mirrors the JSON grammar for the fixture list.
func validJSONNumberText(text string) bool {
	var n json.Number
	return json.Unmarshal([]byte(text), &n) == nil && string(n) == text
}

// Huge exponents cannot make integer checks slow: a union of integer and
// number over many such values validates within a small bound.
func TestJSONIntegerDecisionIsLinearForHugeExponents(t *testing.T) {
	item := "1e-999999"
	values := make([]any, 100000)
	for i := range values {
		values[i] = json.Number(item)
	}
	start := time.Now()
	for _, v := range values {
		if _, ok := exactInteger(v); ok {
			t.Fatal("fraction treated as integer")
		}
	}
	if n, ok := JSONNumberInt64("1e999999999999"); ok {
		t.Fatal("huge exponent accepted as int64", n)
	}
	if !JSONNumberIsInteger("1e999999999999") {
		t.Fatal("huge positive exponent is still an integer")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("integer decisions took %s", elapsed)
	}
}
