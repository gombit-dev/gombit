package dev

import (
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
)

func TestFormatServiceTableIncludesDocsAndOpenAPI(t *testing.T) {
	t.Parallel()

	got := FormatServiceTable(Services{
		Backend:  "http://127.0.0.1:8080",
		Frontend: "http://127.0.0.1:5173",
	})
	wants := []string{
		"Backend",
		"http://127.0.0.1:8080",
		"Frontend",
		"http://127.0.0.1:5173",
		"/openapi.json",
		"/docs",
		"API docs",
		"OpenAPI",
	}
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Fatalf("service table missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "http://127.0.0.1:8080/docs") {
		t.Fatalf("service table missing API docs URL:\n%s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:8080/openapi.json") {
		t.Fatalf("service table missing OpenAPI URL:\n%s", got)
	}
	if strings.Contains(got, "Admin") {
		t.Fatalf("service table included Admin without Admin URL:\n%s", got)
	}
}

func TestFormatServiceTableIncludesAdminWhenSet(t *testing.T) {
	t.Parallel()

	got := FormatServiceTable(Services{
		Backend:  "http://127.0.0.1:8080",
		Frontend: "http://127.0.0.1:5173",
		Admin:    "http://127.0.0.1:8080/admin/",
	})
	if !strings.Contains(got, "Admin") {
		t.Fatalf("service table missing Admin row:\n%s", got)
	}
	if !strings.Contains(got, "http://127.0.0.1:8080/admin/") {
		t.Fatalf("service table missing Admin URL:\n%s", got)
	}
}

func TestAdminURLFromConfig(t *testing.T) {
	t.Parallel()

	cookie := config.Default()
	cookie.Auth.Mode = config.AuthModeCookie
	cookie.Auth.JWTSecret = "dev-secret-at-least-thirty-two-chars"

	jwt := config.Default()
	jwt.Auth.Mode = config.AuthModeJWT
	jwt.Auth.JWTSecret = cookie.Auth.JWTSecret

	disabled := config.Default()
	disabled.Auth.Mode = config.AuthModeCookie

	tests := []struct {
		name string
		cfg  config.Config
		addr string
		want string
	}{
		{
			name: "cookie with secret",
			cfg:  cookie,
			addr: ":8080",
			want: "http://127.0.0.1:8080/admin/",
		},
		{
			name: "cookie honors --http",
			cfg:  cookie,
			addr: "127.0.0.1:9090",
			want: "http://127.0.0.1:9090/admin/",
		},
		{
			name: "jwt omits admin",
			cfg:  jwt,
			addr: ":8080",
			want: "",
		},
		{
			name: "cookie without secret omits admin",
			cfg:  disabled,
			addr: ":8080",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := AdminURLFromConfig(tt.cfg, tt.addr); got != tt.want {
				t.Fatalf("AdminURLFromConfig() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOriginFromAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want string
	}{
		{addr: ":8080", want: "http://127.0.0.1:8080"},
		{addr: "127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{addr: "0.0.0.0:9000", want: "http://127.0.0.1:9000"},
		{addr: "8080", want: "http://127.0.0.1:8080"},
		{addr: "+8080", want: "http://127.0.0.1:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()
			if got := originFromAddr(tt.addr); got != tt.want {
				t.Fatalf("originFromAddr(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}
