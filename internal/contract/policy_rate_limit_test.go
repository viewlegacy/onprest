package contract

import (
	"reflect"
	"testing"
)

// A capability inherits the whole defaults.rate_limit object with per
// normalized, and its own rate_limit replaces that object as a whole.
func TestPolicyDefaultsRateLimitInheritanceAndOverride(t *testing.T) {
	defs := "defaults: {rate_limit: {requests: 60, per: 1m, burst: 10}}\n"
	inherited := compile(t, wasm+"\noutput: {data: {}}", defs)
	if got, want := inherited.Policy["rate_limit"], map[string]any{"requests": int64(60), "per": "1m0s", "burst": int64(10)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("inherited rate_limit=%#v, want %#v", got, want)
	}
	overridden := compile(t, wasm+"\noutput: {data: {}}\npolicy: {rate_limit: {requests: 5, per: 1s, burst: 2}}", defs)
	if got, want := overridden.Policy["rate_limit"], map[string]any{"requests": int64(5), "per": "1s", "burst": int64(2)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("overridden rate_limit=%#v, want %#v", got, want)
	}
	if _, e := Compile([]byte(fixture(wasm+"\noutput: {data: {}}\npolicy: {rate_limit: {requests: 5}}", defs))); e == nil {
		t.Fatal("partial capability rate_limit accepted instead of replacing the whole object")
	}
	if _, ok := compile(t, wasm+"\noutput: {data: {}}", "").Policy["rate_limit"]; ok {
		t.Fatal("rate_limit present without defaults or policy")
	}
}
