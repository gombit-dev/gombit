package dev

import (
	"net"
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
			if _, _, err := net.SplitHostPort(got[0]); err != nil {
				t.Fatalf("child GOMBIT_HTTP_ADDR = %q, the backend cannot listen on it: %v", got[0], err)
			}
		})
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
		if !strings.Contains(err.Error(), "--http") {
			t.Fatalf("normalize(%q) = %v, want it to name --http", addr, err)
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
			if _, _, err := net.SplitHostPort(got[0]); err != nil {
				t.Fatalf("child GOMBIT_HTTP_ADDR = %q, the backend cannot listen on it: %v", got[0], err)
			}
		})
	}

	bad := Options{HTTPAddr: HTTPAddrFromConfig(config.Config{HTTP: config.HTTPConfig{Addr: "99999"}})}
	if err := bad.normalize(); err == nil || !strings.Contains(err.Error(), "--http") {
		t.Fatalf("normalize(GOMBIT_HTTP_ADDR=99999) = %v, want an error naming --http", err)
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
