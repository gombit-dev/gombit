package field

import (
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Patterns copied from Huma 2.39.1 validateFormat so a blank means the same
// thing in admin writes and in the generated request.
var (
	rxHostname       = regexp.MustCompile(`^([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])(\.([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9\-]{0,61}[a-zA-Z0-9]))*$`)
	rxURITemplate    = regexp.MustCompile("^([^{]*({[^}]*})?)*$")
	rxJSONPointer    = regexp.MustCompile("^(?:/(?:[^~/]|~0|~1)*)*$")
	rxRelJSONPointer = regexp.MustCompile("^(?:0|[1-9][0-9]*)(?:#|(?:/(?:[^~/]|~0|~1)*)*)$")
)

// FormatRejects reports whether Huma 2.39.1 validateFormat rejects s for
// format. An unrecognized format is a no-op. uri-reference, iri-reference,
// uri-template, json-pointer, and regex accept "".
func FormatRejects(format, s string) bool {
	switch format {
	case "date-time":
		if _, err := time.Parse(time.RFC3339, s); err == nil {
			return false
		}
		_, err := time.Parse(time.RFC3339Nano, s)
		return err != nil
	case "date-time-http":
		_, err := time.Parse(time.RFC1123, s)
		return err != nil
	case "date":
		_, err := time.Parse(time.DateOnly, s)
		return err != nil
	case "duration":
		_, err := time.ParseDuration(s)
		return err != nil
	case "time":
		if _, err := time.Parse(time.TimeOnly, s); err == nil {
			return false
		}
		_, err := time.Parse("15:04:05Z07:00", s)
		return err != nil
	case "email", "idn-email":
		addr, err := mail.ParseAddress(s)
		return err != nil || addr.Name != "" || addr.Address == "" || strings.TrimSpace(s) != addr.Address
	case "idn-hostname", "hostname":
		return len(s) >= 256 || !rxHostname.MatchString(s)
	case "ipv4":
		addr, err := netip.ParseAddr(s)
		return err != nil || !addr.Is4()
	case "ipv6":
		addr, err := netip.ParseAddr(s)
		return err != nil || !addr.Is6() || addr.Is4In6()
	case "ip":
		_, err := netip.ParseAddr(s)
		return err != nil
	case "uri", "iri":
		u, err := url.Parse(s)
		return err != nil || s == "" || u.Scheme == ""
	case "uri-reference", "iri-reference":
		_, err := url.Parse(s)
		return err != nil
	case "uri-template":
		u, err := url.Parse(s)
		if err != nil {
			return true
		}
		return !rxURITemplate.MatchString(u.Path)
	case "uuid":
		return !uuidOK(s)
	case "json-pointer":
		return !rxJSONPointer.MatchString(s)
	case "relative-json-pointer":
		return !rxRelJSONPointer.MatchString(s)
	case "regex":
		_, err := regexp.Compile(s)
		return err != nil
	default:
		return false
	}
}

func uuidOK(s string) bool {
	switch len(s) {
	case 36 + 9:
		if !strings.EqualFold(s[:9], "urn:uuid:") {
			return false
		}
		s = s[9:]
	case 36 + 2:
		if s[0] != '{' || s[len(s)-1] != '}' {
			return false
		}
		s = s[1 : len(s)-1]
	case 32:
		for i := 0; i < len(s); i += 2 {
			if _, err := strconv.ParseUint(s[i:i+2], 16, 8); err != nil {
				return false
			}
		}
		return true
	case 36:
	default:
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for _, x := range []int{0, 2, 4, 6, 9, 11, 14, 16, 19, 21, 24, 26, 28, 30, 32, 34} {
		if _, err := strconv.ParseUint(s[x:x+2], 16, 8); err != nil {
			return false
		}
	}
	return true
}
