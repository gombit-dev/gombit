package framework

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestCustomRouterOmitsRuntimeBodyLimit locks the WithRouter boundary (issue
// #271 review): framework.New installs the runtime middleware stack only on the
// router it builds, so a custom router gets no request_body_limit (nor any other
// runtime layer). The 8MiB JSON body-size bound is a property of the default
// stack, not of every Gombit app — this keeps the security docs from drifting to
// "every app / every route".
func TestCustomRouterOmitsRuntimeBodyLimit(t *testing.T) {
	app := newTestApp(t, WithRouter(gin.New()))

	handlerRan := false
	app.Router().POST("/echo", func(c *gin.Context) {
		handlerRan = true
		_, _ = io.ReadAll(c.Request.Body)
		c.Status(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(oversizedJSONBody()))
	req.Header.Set("Content-Type", "application/json")
	app.Router().ServeHTTP(rec, req)

	if !handlerRan {
		t.Fatal("custom-router handler did not run; expected no framework body-size limit under WithRouter")
	}
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatal("custom router enforced the runtime body-size limit; WithRouter apps own their own middleware")
	}
}

func TestDefaultRouterMountsOnlyFrameworkEndpoints(t *testing.T) {
	app := newTestApp(t)

	var got []string
	for _, route := range app.Router().Routes() {
		got = append(got, route.Method+" "+route.Path)
	}
	sort.Strings(got)

	want := []string{
		"GET /docs",
		"GET /livez",
		"GET /metrics",
		"GET /openapi-3.0.json",
		"GET /openapi-3.0.yaml",
		"GET /openapi.json",
		"GET /openapi.yaml",
		"GET /readyz",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Router().Routes() = %v, want %v", got, want)
	}
}

func TestApplicationOwnedRouteRegistrationComposesIndependently(t *testing.T) {
	app := newTestApp(t)

	var (
		mu    sync.Mutex
		order []string
	)
	record := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, value)
	}

	ping := app.Router().Group("/ping")
	ping.Use(func(c *gin.Context) {
		record("ping-middleware")
		c.Next()
	})
	ping.GET("", func(c *gin.Context) {
		record("ping-handler")
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}})
	})

	echo := app.Router().Group("/echo")
	echo.Use(func(c *gin.Context) {
		record("echo-middleware")
		c.Next()
	})
	echo.POST("", func(c *gin.Context) {
		record("echo-handler")
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"status": "ok"}})
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, app)
	}()
	t.Cleanup(func() {
		cancel()
		if err := waitRun(done); err != nil {
			t.Fatalf("RunContext() error = %v, want nil", err)
		}
	})

	waitForHTTP(t, app, "/livez")

	pingResp := getHTTP(t, app, "/ping")
	_, _ = io.Copy(io.Discard, pingResp.Body)
	_ = pingResp.Body.Close()
	if pingResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ping status = %d, want %d", pingResp.StatusCode, http.StatusOK)
	}

	mu.Lock()
	afterPing := append([]string(nil), order...)
	mu.Unlock()

	wantAfterPing := []string{"ping-middleware", "ping-handler"}
	if !reflect.DeepEqual(afterPing, wantAfterPing) {
		t.Fatalf("order after GET /ping = %v, want %v (no cross-module leakage, deterministic order)", afterPing, wantAfterPing)
	}

	echoResp := postHTTP(t, app, "/echo")
	_, _ = io.Copy(io.Discard, echoResp.Body)
	_ = echoResp.Body.Close()
	if echoResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /echo status = %d, want %d", echoResp.StatusCode, http.StatusOK)
	}

	mu.Lock()
	afterEcho := append([]string(nil), order...)
	mu.Unlock()

	wantAfterEcho := []string{"ping-middleware", "ping-handler", "echo-middleware", "echo-handler"}
	if !reflect.DeepEqual(afterEcho, wantAfterEcho) {
		t.Fatalf("order after POST /echo = %v, want %v (echo middleware must not have fired for /ping)", afterEcho, wantAfterEcho)
	}
}

func postHTTP(t *testing.T, app *App, path string) *http.Response {
	t.Helper()

	client := &http.Client{Timeout: 100 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		addr := app.Addr()
		if addr == "" {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		resp, err := client.Post("http://"+addr+path, "application/json", nil)
		if err == nil {
			return resp
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for POST %s on %s", path, app.Addr())
	return nil
}
