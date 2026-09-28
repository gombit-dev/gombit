package cache

import (
	"testing"
	"time"

	"github.com/gombit-dev/gombit/config"
)

func TestNewRedisClientUsesTypedConfig(t *testing.T) {
	client, err := NewRedisClient(config.RedisConfig{
		Addr:         "127.0.0.1:6379",
		Username:     "user",
		DB:           2,
		DialTimeout:  time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 3 * time.Second,
		TLS:          true,
	})
	if err != nil {
		t.Fatalf("NewRedisClient() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
	})

	options := client.Options()
	if options.Addr != "127.0.0.1:6379" {
		t.Fatalf("Addr = %q, want 127.0.0.1:6379", options.Addr)
	}
	if options.Username != "user" {
		t.Fatalf("Username = %q, want user", options.Username)
	}
	if options.DB != 2 {
		t.Fatalf("DB = %d, want 2", options.DB)
	}
	if !options.ContextTimeoutEnabled {
		t.Fatal("ContextTimeoutEnabled = false, want the caller's deadline honored")
	}
	if options.DialTimeout != time.Second {
		t.Fatalf("DialTimeout = %s, want 1s", options.DialTimeout)
	}
	if options.ReadTimeout != 2*time.Second {
		t.Fatalf("ReadTimeout = %s, want 2s", options.ReadTimeout)
	}
	if options.WriteTimeout != 3*time.Second {
		t.Fatalf("WriteTimeout = %s, want 3s", options.WriteTimeout)
	}
	if options.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want configured TLS")
	}
}
