//go:build windows

package providers

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The owner and DACL decision mirrors the Unix owner/mode check: only the
// agent user, SYSTEM or Administrators may own the object or be granted
// access, and the DACL must not inherit entries.
func TestPrivateSecurityOwnerAndDACLDecision(t *testing.T) {
	sid := func(s string) *windows.SID {
		v, e := windows.StringToSid(s)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	user := sid("S-1-5-21-1000-1000-1000-1001")
	other := sid("S-1-5-21-1000-1000-1000-1002")
	system, admins, everyone := sid("S-1-5-18"), sid("S-1-5-32-544"), sid("S-1-1-0")
	allow := func(s *windows.SID) privateACE { return privateACE{Type: windows.ACCESS_ALLOWED_ACE_TYPE, SID: s} }
	deny := func(s *windows.SID) privateACE { return privateACE{Type: windows.ACCESS_DENIED_ACE_TYPE, SID: s} }
	trusted := []privateACE{allow(user), allow(system), allow(admins)}
	for _, c := range []struct {
		name      string
		protected bool
		owner     *windows.SID
		hasDACL   bool
		aces      []privateACE
		ok        bool
	}{
		{"user owner, protected trusted DACL", true, user, true, trusted, true},
		{"SYSTEM owner", true, system, true, trusted, true},
		{"Administrators owner", true, admins, true, trusted, true},
		{"deny entries are allowed", true, user, true, append([]privateACE{deny(everyone)}, trusted...), true},
		{"empty protected DACL", true, user, true, nil, true},
		{"other user owns it", true, other, true, trusted, false},
		{"Everyone owns it", true, everyone, true, trusted, false},
		{"no owner", true, nil, true, trusted, false},
		{"inherited DACL", false, user, true, trusted, false},
		{"null DACL grants everyone", true, user, false, nil, false},
		{"other user granted", true, user, true, append(trusted, allow(other)), false},
		{"Everyone granted", true, user, true, append(trusted, allow(everyone)), false},
		{"unsupported (object) ACE type", true, user, true, []privateACE{{Type: 5, SID: user}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := privateSecurity(c.protected, c.owner, c.hasDACL, c.aces, user)
			if (e == nil) != c.ok {
				t.Fatalf("accepted=%v want %v: %v", e == nil, c.ok, e)
			}
		})
	}
}
