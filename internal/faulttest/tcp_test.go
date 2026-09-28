package faulttest_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/internal/faulttest"
)

// echoServer answers every line with the same line.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

func roundTrip(conn net.Conn, line string, timeout time.Duration) (string, error) {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, line+"\n"); err != nil {
		return "", err
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	return strings.TrimSuffix(got, "\n"), err
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestTCPProxyForwards(t *testing.T) {
	proxy := faulttest.NewTCPProxy(t, echoServer(t))
	if got, err := roundTrip(dial(t, proxy.Addr()), "hello", 5*time.Second); err != nil || got != "hello" {
		t.Fatalf("round trip = %q, %v", got, err)
	}
	if proxy.Connections() != 1 {
		t.Fatalf("Connections = %d, want 1", proxy.Connections())
	}
}

func TestTCPProxyHoldStallsUntilHeal(t *testing.T) {
	proxy := faulttest.NewTCPProxy(t, echoServer(t))
	conn := dial(t, proxy.Addr())
	proxy.Hold()
	done := make(chan error, 1)
	go func() {
		_, err := roundTrip(conn, "held", 5*time.Second)
		done <- err
	}()
	<-proxy.Held() // the write reached the proxy and stalled
	select {
	case err := <-done:
		t.Fatalf("a held round trip returned (%v)", err)
	default:
	}
	if err := proxy.Heal(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("round trip after Heal = %v", err)
	}
}

func TestTCPProxyHoldOutlastsADeadline(t *testing.T) {
	proxy := faulttest.NewTCPProxy(t, echoServer(t))
	conn := dial(t, proxy.Addr())
	proxy.Hold()
	start := time.Now()
	_, err := roundTrip(conn, "late", 100*time.Millisecond)
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("held round trip = %v, want the client's timeout", err)
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("the client waited %s past its 100ms deadline", took)
	}
}

func TestTCPProxyCutAndReset(t *testing.T) {
	for name, fault := range map[string]func(*faulttest.TCPProxy){
		"cut":   (*faulttest.TCPProxy).Cut,
		"reset": (*faulttest.TCPProxy).Reset,
	} {
		t.Run(name, func(t *testing.T) {
			proxy := faulttest.NewTCPProxy(t, echoServer(t))
			conn := dial(t, proxy.Addr())
			if _, err := roundTrip(conn, "before", 5*time.Second); err != nil {
				t.Fatal(err)
			}
			fault(proxy)
			_, err := roundTrip(conn, "after", 5*time.Second)
			if err == nil {
				t.Fatal("a round trip on a dropped connection succeeded")
			}
			// A reset reads as ECONNRESET (or EPIPE on the write); a FIN (Cut)
			// reads as EOF, which must not pass for a reset.
			if name == "reset" && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE) {
				t.Fatalf("after reset = %v, want ECONNRESET or EPIPE, not a plain close", err)
			}
			if name == "cut" && !errors.Is(err, io.EOF) {
				t.Fatalf("after cut = %v, want EOF (a FIN)", err)
			}
			// Recovery: a new connection works.
			if got, err := roundTrip(dial(t, proxy.Addr()), "again", 5*time.Second); err != nil || got != "again" {
				t.Fatalf("a new connection after %s = %q, %v", name, got, err)
			}
		})
	}
}

func TestTCPProxyRefuseAndHeal(t *testing.T) {
	proxy := faulttest.NewTCPProxy(t, echoServer(t))
	proxy.Refuse()
	start := time.Now()
	if _, err := net.DialTimeout("tcp", proxy.Addr(), 5*time.Second); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("dial while refusing = %v, want connection refused", err)
	}
	if took := time.Since(start); took >= time.Second {
		t.Fatalf("a refused dial took %s, want it at once", took)
	}
	if err := proxy.Heal(); err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(dial(t, proxy.Addr()), "back", 5*time.Second); err != nil || got != "back" {
		t.Fatalf("round trip after Heal = %q, %v", got, err)
	}
}

func TestPostgresDSNVia(t *testing.T) {
	got, err := faulttest.PostgresDSNVia("postgres://u@db.example:5432/app?sslmode=disable", "127.0.0.1:6000")
	if err != nil || got != "postgres://u@127.0.0.1:6000/app?sslmode=disable" {
		t.Fatalf("PostgresDSNVia = %q, %v", got, err)
	}
	for dsn, want := range map[string]string{
		"postgres://u@db.example/app":      "db.example:5432",
		"postgres://u@db.example:6543/app": "db.example:6543",
		"postgres://u@[::1]/app":           "[::1]:5432",
		"postgres://u@[::1]:6543/app":      "[::1]:6543",
	} {
		if hp, err := faulttest.PostgresHostPort(dsn); err != nil || hp != want {
			t.Fatalf("PostgresHostPort(%s) = %q, %v; want %q", dsn, hp, err, want)
		}
	}
	if _, err := faulttest.PostgresDSNVia("host=x user=y", "a:1"); err == nil {
		t.Fatal("a key=value DSN was accepted")
	}
}
