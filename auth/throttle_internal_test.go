package auth

import "testing"

func TestAddressSubject(t *testing.T) {
	for in, want := range map[string]string{
		"198.51.100.7":            "198.51.100.7",
		"::ffff:198.51.100.7":     "198.51.100.7",
		"2001:db8:1:2::1":         "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:1:2:3": "2001:db8:1:2::/64",
		"fe80::1%eth0":            "fe80::/64",
		"not an address":          "not an address",
	} {
		if got := addressSubject(in); got != want {
			t.Errorf("addressSubject(%q) = %q, want %q", in, got, want)
		}
	}
}
