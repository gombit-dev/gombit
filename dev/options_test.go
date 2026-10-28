package dev

import (
	"bytes"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
)

func TestValidateFlags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		httpAddr     string
		frontendHost string
		frontendPort int
		poll         time.Duration
		clientOut    string
		want         string
	}{
		{
			name:         "empty http",
			httpAddr:     "   ",
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--http",
		},
		{
			name:         "invalid http",
			httpAddr:     "not-an-addr",
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--http",
		},
		{
			name:         "http port above 65535",
			httpAddr:     ":99999",
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--http",
		},
		{
			name:         "negative http port",
			httpAddr:     ":-1",
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--http",
		},
		{
			name:         "zero http port with a host",
			httpAddr:     "127.0.0.1:0",
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--http",
		},
		{
			name:         "empty frontend host",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: " ",
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--frontend-host",
		},
		{
			name:         "zero frontend port",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: DefaultFrontendHost,
			frontendPort: 0,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--frontend-port",
		},
		{
			name:         "negative frontend port",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: DefaultFrontendHost,
			frontendPort: -1,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--frontend-port",
		},
		{
			name:         "frontend port too large",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: DefaultFrontendHost,
			frontendPort: 70000,
			poll:         DefaultPollInterval,
			clientOut:    DefaultClientOut,
			want:         "--frontend-port",
		},
		{
			name:         "zero poll",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         0,
			clientOut:    DefaultClientOut,
			want:         "--poll",
		},
		{
			name:         "empty client out",
			httpAddr:     DefaultHTTPAddr,
			frontendHost: DefaultFrontendHost,
			frontendPort: DefaultFrontendPort,
			poll:         DefaultPollInterval,
			clientOut:    " ",
			want:         "--client-out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateFlags(tt.httpAddr, tt.frontendHost, tt.frontendPort, tt.poll, tt.clientOut)
			if err == nil {
				t.Fatal("ValidateFlags() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateFlags() error = %q, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateFlagsOK(t *testing.T) {
	t.Parallel()
	err := ValidateFlags(DefaultHTTPAddr, DefaultFrontendHost, DefaultFrontendPort, DefaultPollInterval, DefaultClientOut)
	if err != nil {
		t.Fatalf("ValidateFlags() error = %v", err)
	}
}

// normalize has to keep the address it validated: the backend child gets
// opts.HTTPAddr verbatim as GOMBIT_HTTP_ADDR, and a bare "8080" is
// "listen tcp: address 8080: missing port in address" there.
func TestNormalizeKeepsTheHTTPAddrTheChildCanListenOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare port", in: "8080", want: ":8080"},
		{name: "already normalised", in: ":8080", want: ":8080"},
		{name: "signed port", in: "+8080", want: ":8080"},
		{name: "zero-padded port", in: "08080", want: ":8080"},
		{name: "with a host", in: "127.0.0.1:9090", want: "127.0.0.1:9090"},
		{name: "ipv6", in: "[::1]:8080", want: "[::1]:8080"},
		{name: "surrounding whitespace", in: " 8080 ", want: ":8080"},
		{name: "unset", in: "", want: DefaultHTTPAddr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{HTTPAddr: tt.in}
			if err := opts.normalize(); err != nil {
				t.Fatalf("normalize(%q) = %v, want nil", tt.in, err)
			}
			if opts.HTTPAddr != tt.want {
				t.Fatalf("opts.HTTPAddr = %q, want %q", opts.HTTPAddr, tt.want)
			}

			got := envKeyValues(childEnv(nil, opts, originFromAddr(opts.HTTPAddr)), "GOMBIT_HTTP_ADDR")
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("child GOMBIT_HTTP_ADDR = %v, want exactly [%s]", got, tt.want)
			}
			assertAddrUsable(t, got[0])
		})
	}
}

// assertAddrUsable checks the address the way each consumer reads it: the
// backend binds it with net.Listen, and the service table, GOMBIT_DEV_BACKEND
// and the OpenAPI watcher all hand the origin built from it to net/url.
// net.SplitHostPort alone would pass on ":99999" and ":+8080" — both parse
// there and neither survives the second hop.
func assertAddrUsable(t *testing.T, addr string) {
	t.Helper()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("the backend cannot listen on %q: %v", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		t.Fatalf("port %q of %q is not one net.Listen can bind: %v", port, addr, err)
	}
	// Bind the same host on an ephemeral port: 8080 is not free on every
	// runner, and the host form is what this assertion can prove.
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("the backend cannot listen on %q: %v", addr, err)
	}
	defer func() {
		_ = ln.Close()
	}()

	origin := originFromAddr(addr)
	u, err := url.Parse(origin + "/openapi.json")
	if err != nil {
		t.Fatalf("originFromAddr(%q) = %q, which the OpenAPI watcher cannot fetch: %v", addr, origin, err)
	}
	if u.Host == "" || u.Path != "/openapi.json" {
		t.Fatalf("originFromAddr(%q) = %q, want a host and the polled path", addr, origin)
	}
	backend := envKeyValues(childEnv(nil, Options{HTTPAddr: addr}, origin), "GOMBIT_DEV_BACKEND")
	if len(backend) != 1 {
		t.Fatalf("child GOMBIT_DEV_BACKEND = %v, want exactly one value", backend)
	}
	if _, err := url.Parse(backend[0]); err != nil {
		t.Fatalf("child GOMBIT_DEV_BACKEND = %q, which Vite cannot parse as a proxy target: %v", backend[0], err)
	}
}

// A rewritten address is the case dev must not keep quiet about: dev can hand
// the child ":8080" for a value written as "8080", but the same .env read by
// go run ./cmd/server is not a listen address at all. normalize says the
// rewrite out loud so the user fixes the value rather than the symptom.
func TestNormalizeReportsAnAddrItHadToRewrite(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	opts := Options{
		HTTPAddr: HTTPAddrFromConfig(config.Config{HTTP: config.HTTPConfig{Addr: "8080"}}),
		Stderr:   &stderr,
	}
	if err := opts.normalize(); err != nil {
		t.Fatalf("normalize() = %v, want nil", err)
	}
	if opts.HTTPAddr != ":8080" {
		t.Fatalf("opts.HTTPAddr = %q, want %q", opts.HTTPAddr, ":8080")
	}
	got := stderr.String()
	if !strings.Contains(got, `"8080"`) || !strings.Contains(got, `":8080"`) {
		t.Fatalf("stderr = %q, want it to name both the value written and the one dev used", got)
	}
	if !strings.Contains(got, "GOMBIT_HTTP_ADDR") {
		t.Fatalf("stderr = %q, want it to point at the config key to fix", got)
	}
	// The same normalize runs for `--http 8080`, which docs/cli.md recommends.
	// A notice that reads as an instruction to edit .env sends the user to a file
	// they never wrote, so it has to stay conditional and name no file (#513 review).
	if strings.Contains(got, ".env") {
		t.Fatalf("stderr = %q, want no instruction to edit a file dev did not read", got)
	}
	if !strings.Contains(got, "if it came from") {
		t.Fatalf("stderr = %q, want it to condition the advice on where the value came from", got)
	}

	stderr.Reset()
	already := Options{HTTPAddr: ":8080", Stderr: &stderr}
	if err := already.normalize(); err != nil {
		t.Fatalf("normalize() = %v, want nil", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing for an address dev did not have to rewrite", stderr.String())
	}
}

func TestNormalizeRejectsAPortTheBackendCannotBind(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{":99999", ":-1", ":0", "127.0.0.1:70000"} {
		opts := Options{HTTPAddr: addr}
		err := opts.normalize()
		if err == nil {
			t.Fatalf("normalize(%q) = nil, want an error before anything starts", addr)
		}
		if !strings.Contains(err.Error(), addr) {
			t.Fatalf("normalize(%q) = %v, want it to name the address it rejected", addr, err)
		}
	}
}

// The issue's second route: the address arrives from GOMBIT_HTTP_ADDR through
// config instead of --http, and reaches the child the same normalised way.
func TestNormalizeKeepsTheConfiguredHTTPAddrListenable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare port", in: "8080", want: ":8080"},
		{name: "unset", in: "", want: DefaultHTTPAddr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{HTTPAddr: HTTPAddrFromConfig(config.Config{HTTP: config.HTTPConfig{Addr: tt.in}})}
			if err := opts.normalize(); err != nil {
				t.Fatalf("normalize() = %v, want nil", err)
			}
			got := envKeyValues(childEnv(nil, opts, originFromAddr(opts.HTTPAddr)), "GOMBIT_HTTP_ADDR")
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("child GOMBIT_HTTP_ADDR = %v, want exactly [%s]", got, tt.want)
			}
			assertAddrUsable(t, got[0])
		})
	}

	bad := Options{HTTPAddr: HTTPAddrFromConfig(config.Config{HTTP: config.HTTPConfig{Addr: "99999"}})}
	err := bad.normalize()
	if err == nil {
		t.Fatal("normalize(GOMBIT_HTTP_ADDR=99999) = nil, want an error before anything starts")
	}
	if !strings.Contains(err.Error(), "99999") {
		t.Fatalf("normalize(GOMBIT_HTTP_ADDR=99999) = %v, want it to name the address", err)
	}
	// The value arrived through config, not the flag: an error that reads "pass
	// --http" describes a route this call did not take (#513 review).
	if strings.Contains(err.Error(), "--http") {
		t.Fatalf("normalize(GOMBIT_HTTP_ADDR=99999) = %v, want no claim about a flag that was not used", err)
	}
}

func TestAPIPrefixFromConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "/api/v1"},
		{name: "spaces", in: "   ", want: "/api/v1"},
		{name: "default", in: "/api/v1", want: "/api/v1"},
		{name: "trailing slash", in: "/svc/v2/", want: "/svc/v2"},
		{name: "custom", in: "/svc/v2", want: "/svc/v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.Config{}
			cfg.API.Prefix = tt.in
			if got := APIPrefixFromConfig(cfg); got != tt.want {
				t.Fatalf("APIPrefixFromConfig(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
