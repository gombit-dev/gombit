package auth

import (
	"errors"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
)

func TestWithBearerChallenge(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "unauthorized",
			err:  contract.Authentication("session has ended"),
			want: `Bearer realm="api"`,
		},
		{
			name: "non-unauthorized",
			err:  contract.NotFound("session not found"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := withBearerChallenge(tt.err)
			var headersErr huma.HeadersError
			if !errors.As(err, &headersErr) {
				if tt.want == "" {
					return
				}
				t.Fatalf("error has no response headers; want WWW-Authenticate %q", tt.want)
			}
			if got := headersErr.GetHeaders().Get("WWW-Authenticate"); got != tt.want {
				t.Fatalf("WWW-Authenticate = %q, want %q", got, tt.want)
			}
			if !errors.Is(err, tt.err) {
				t.Fatal("wrapped error did not preserve its cause")
			}
		})
	}
}
