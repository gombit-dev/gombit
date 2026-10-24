package client

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/framework"
)

// The prefix is what every path in the OpenAPI document and every route the app
// registers is built from, so the rule about it is only worth having if the two
// agree. That is what this checks: it puts a prefix in the environment the way an
// operator does, builds the app the loaded configuration describes, and asks for
// each path the resulting document advertises. A prefix of /svc/ used to
// advertise /svc//widgets, which nothing answered.
func TestServedRoutesAnswerEveryDocumentedPath(t *testing.T) {
	tests := []struct {
		name       string
		envPrefix  string // GOMBIT_API_PREFIX, as an operator writes it
		wantPrefix string // what the loaded configuration stores
	}{
		{name: "a trailing slash", envPrefix: "/svc/", wantPrefix: "/svc"},
		{name: "several trailing slashes", envPrefix: "/svc///", wantPrefix: "/svc"},
		{name: "space around it", envPrefix: " /svc/ ", wantPrefix: "/svc"},
		{name: "already canonical", envPrefix: "/api/v1", wantPrefix: "/api/v1"},
		{name: "sub-delimiters", envPrefix: "/api/v1+beta", wantPrefix: "/api/v1+beta"},
		{name: "non-ASCII", envPrefix: "/café", wantPrefix: "/café"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GOMBIT_ENV", "test")
			t.Setenv("GOMBIT_API_PREFIX", tt.envPrefix)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() with GOMBIT_API_PREFIX=%q error = %v", tt.envPrefix, err)
			}
			if cfg.API.Prefix != tt.wantPrefix {
				t.Fatalf("API.Prefix = %q, want %q", cfg.API.Prefix, tt.wantPrefix)
			}

			app, err := framework.New(framework.WithConfig(cfg))
			if err != nil {
				t.Fatalf("framework.New() error = %v", err)
			}
			registerSampleWidgetRoutes(app.API(), app.Config().API.Prefix)

			widgets := tt.wantPrefix + "/widgets"
			paths := app.API().OpenAPI().Paths
			if _, ok := paths[widgets]; !ok {
				t.Fatalf("document paths = %v, want %q", paths, widgets)
			}
			for path, item := range paths {
				if item == nil || item.Get == nil {
					continue
				}
				request := fillPathParams(path)
				rec := httptest.NewRecorder()
				// A client fills the document's {parameters} before asking, so
				// this is the request the document tells an app to make.
				app.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, request, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("the document advertises %s: GET %s = %d, want a route that answers", path, request, rec.Code)
				}
			}
		})
	}
}

// fillPathParams substitutes the sample widget's id for every {parameter} in a
// documented path, the way a client built from the document does.
func fillPathParams(path string) string {
	for strings.Contains(path, "{") {
		start := strings.IndexByte(path, '{')
		end := strings.IndexByte(path[start:], '}')
		if end < 0 {
			break
		}
		path = path[:start] + "widget-1" + path[start+end+1:]
	}
	return path
}

// A prefix set in code reaches Validate with nothing normalising it first, so an
// app built from one whose document its routes would not answer has to refuse to
// start rather than serve 404 on every documented path. This is the behaviour the
// release notes declare as breaking, so it is pinned at the door every app comes
// through, with the canonical prefix as the control.
func TestNewRefusesAPrefixNoRouteAnswers(t *testing.T) {
	tests := []struct {
		prefix   string
		accepted bool
		why      string
	}{
		{prefix: "/api/v1", accepted: true, why: "canonical"},
		{prefix: "/svc/", why: "advertises /svc//widgets"},
		{prefix: "/api/../v1", why: "advertises the path Gin collapses away"},
		{prefix: "/svc//api", why: "advertises a doubled slash"},
		{prefix: "/api/:id", why: "Gin reads it as a parameter and answers other paths"},
		{prefix: "/api/{id}", why: "the same, in Gin's brace syntax"},
		{prefix: "/api/*widgets", why: "Gin's catch-all, which it refuses mid-path"},
		{prefix: `/api\id`, why: "Gin reads a malformed escape"},
		{prefix: "/api/v1%x", why: "not a URL a client can parse"},
		{prefix: "/api/v1?x", why: "the query never reaches the path"},
		{prefix: "/api/v1#x", why: "the fragment never reaches the server"},
		{prefix: "/api/v 1", why: "a client has to encode a space to send it"},
		{prefix: "/api/v1\x01", why: "net/url refuses to parse the key"},
		{prefix: "/api/v1+beta", accepted: true, why: "registers, advertises and answers it"},
		{prefix: "/café", accepted: true, why: "the same, through UTF-8 percent-encoding"},
	}

	for _, tt := range tests {
		cfg := config.DefaultFor(config.EnvironmentTest)
		cfg.API.Prefix = tt.prefix
		_, err := framework.New(framework.WithConfig(cfg))
		if tt.accepted {
			if err != nil {
				t.Fatalf("framework.New() with prefix %q (%s) error = %v, want nil", tt.prefix, tt.why, err)
			}
			continue
		}

		var errs config.FieldErrors
		if !errors.As(err, &errs) {
			t.Fatalf("framework.New() with prefix %q (%s) error = %v, want field errors", tt.prefix, tt.why, err)
		}
		found := false
		for _, field := range errs {
			found = found || field.Field == "API.Prefix"
		}
		if !found {
			t.Fatalf("framework.New() with prefix %q errors = %#v, want one for API.Prefix", tt.prefix, []config.FieldError(errs))
		}
	}
}
