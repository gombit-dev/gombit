package auth

import (
	"context"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/gombit-dev/gombit/config"
)

// A session keeps a short description of the device that started it, for
// a user to recognize it in GET /auth/sessions (AUTH-12, #662): the
// sign-in request's User-Agent, its client address as much of it as
// config.AuthConfig.SessionClientAddress keeps (its network by default),
// and the location an application's SessionLocator names. The login
// records it on the session's first refresh_tokens row and every rotation
// copies it, so it describes the sign-in, not the latest refresh. Nothing
// else about the device is stored; the framework does no geolocation.

// The device columns' sizes on refresh_tokens.
const (
	sessionUserAgentSize     = 255
	sessionClientAddressSize = 64
	sessionLocationSize      = 128
)

// Coarse client addresses keep the network: an IPv4 /24, an IPv6 /48 (a
// site's usual allocation).
const (
	coarseIPv4Bits = 24
	coarseIPv6Bits = 48
)

// SessionLocator names where a session's sign-in came from, for GET
// /auth/sessions to show ("Lisbon, PT", say), from the client's full
// address, whatever config.AuthConfig.SessionClientAddress stores. It runs
// once per sign-in, in the request, before the session is written: it must
// be quick (a local GeoIP database, not a network call) and return "" when
// it does not know. Its answer is stored cut to 128 bytes. The framework
// ships none: an application that wants locations passes one with
// WithSessionLocator, and with it the decision to derive them.
type SessionLocator func(ctx context.Context, addr netip.Addr) string

// WithSessionLocator sets the SessionLocator that names a session's
// location when it starts. Without one, sessions have no location.
func WithSessionLocator(l SessionLocator) Option {
	return func(s *Service) { s.locator = l }
}

// sessionDevice is what a session keeps about the device that started it.
type sessionDevice struct {
	userAgent     string
	clientAddress string
	location      string
}

// deviceOf is the device a refresh token row's session recorded, for its
// rotation to carry on.
func deviceOf(row RefreshToken) sessionDevice {
	return sessionDevice{userAgent: row.UserAgent, clientAddress: row.ClientAddress, location: row.Location}
}

// deviceFrom describes the device of the request ctx carries, from what
// CaptureRequestInfo recorded: nothing outside a request (IssueTokens from
// application code, say).
func (s *Service) deviceFrom(ctx context.Context) sessionDevice {
	info, ok := ctx.Value(requestInfoKey{}).(requestInfo)
	if !ok {
		return sessionDevice{}
	}
	device := sessionDevice{userAgent: storableText(info.userAgent, sessionUserAgentSize)}
	addr, err := netip.ParseAddr(info.ip)
	if err != nil {
		return device
	}
	addr = addr.Unmap().WithZone("")
	device.clientAddress = sessionClientAddress(addr, s.cfg.EffectiveSessionClientAddress())
	if s.locator != nil {
		device.location = storableText(s.locator(ctx, addr), sessionLocationSize)
	}
	return device
}

// sessionClientAddress is how much of addr a session keeps under mode: its
// network in CIDR notation (coarse), all of it (full), or nothing (none).
func sessionClientAddress(addr netip.Addr, mode config.SessionClientAddress) string {
	switch mode {
	case config.SessionClientAddressFull:
		return addr.String()
	case config.SessionClientAddressNone:
		return ""
	}
	bits := coarseIPv6Bits
	if addr.Is4() {
		bits = coarseIPv4Bits
	}
	return netip.PrefixFrom(addr, bits).Masked().String()
}

// storableText is s as every supported database stores it in a column of
// size bytes: valid UTF-8 (MySQL refuses anything else in utf8mb4), without
// control characters (PostgreSQL refuses NUL, and none belongs in a label),
// cut at a character boundary.
func storableText(s string, size int) string {
	s = strings.ToValidUTF8(s, string(utf8.RuneError))
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return truncate(strings.TrimSpace(s), size)
}
