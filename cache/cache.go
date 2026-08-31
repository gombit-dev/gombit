package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gombit-dev/gombit/config"
	"github.com/redis/go-redis/v9"
)

// Cache is the value-oriented cache contract used by framework features.
type Cache interface {
	Get(ctx context.Context, key string, dst any) (bool, error)
	Set(ctx context.Context, key string, value any, ttl time.Duration) error
	Delete(ctx context.Context, keys ...string) error
	Increment(ctx context.Context, key string, delta int64) (int64, error)
}

// Driver names an opened cache driver.
type Driver string

const (
	// DriverMemory stores cache values in process memory.
	DriverMemory Driver = Driver(config.CacheDriverMemory)
	// DriverRedis stores cache values in Redis.
	DriverRedis Driver = Driver(config.CacheDriverRedis)
	// DriverNoop disables persistence while preserving cache call sites.
	DriverNoop Driver = Driver(config.CacheDriverNoop)
)

// Store is an opened cache with driver metadata and optional Redis escape hatch.
type Store struct {
	impl   Cache
	driver Driver
	redis  *redis.Client
	close  func() error
}

// Open opens the configured cache driver.
func Open(cfg config.CacheConfig) (*Store, error) {
	if err := config.ValidateCache(cfg); err != nil {
		return nil, err
	}

	var impl Cache
	var client *redis.Client
	var closeFn func() error

	switch driver := Driver(cfg.Driver); driver {
	case DriverMemory:
		mem := NewMemory(WithJanitor(memoryJanitorInterval))
		impl = mem
		closeFn = mem.Close
	case DriverRedis:
		var err error
		client, err = NewRedisClient(cfg.Redis)
		if err != nil {
			return nil, err
		}
		impl = NewRedis(client)
		closeFn = client.Close
	case DriverNoop:
		impl = Noop{}
	default:
		return nil, fmt.Errorf("cache: unsupported driver %q", driver)
	}

	if namespace := strings.TrimSpace(cfg.Namespace); namespace != "" {
		impl = Namespace(namespace, impl)
	}

	return &Store{
		impl:   impl,
		driver: Driver(cfg.Driver),
		redis:  client,
		close:  closeFn,
	}, nil
}

// NewRedisClient builds a go-redis client from typed configuration.
func NewRedisClient(cfg config.RedisConfig) (*redis.Client, error) {
	options := &redis.Options{
		Addr:         cfg.Addr,
		Username:     cfg.Username,
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  cfg.DialTimeout,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}
	if cfg.TLS {
		options.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.TLSInsecure {
			options.TLSConfig.InsecureSkipVerify = true // #nosec G402 -- explicit config opt-in for development TLS.
		}
	}
	return redis.NewClient(options), nil
}

// Driver returns the configured driver.
func (s *Store) Driver() Driver {
	if s == nil {
		return ""
	}
	return s.driver
}

// Redis returns the underlying go-redis client when the Redis driver is enabled.
func (s *Store) Redis() *redis.Client {
	if s == nil {
		return nil
	}
	return s.redis
}

// Close closes driver-owned resources.
func (s *Store) Close() error {
	if s == nil || s.close == nil {
		return nil
	}
	if err := s.close(); err != nil {
		return fmt.Errorf("cache: close: %w", err)
	}
	return nil
}

// Get implements Cache.
func (s *Store) Get(ctx context.Context, key string, dst any) (bool, error) {
	if s == nil || s.impl == nil {
		return false, errors.New("cache: nil store")
	}
	return s.impl.Get(ctx, key, dst)
}

// Set implements Cache.
func (s *Store) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if s == nil || s.impl == nil {
		return errors.New("cache: nil store")
	}
	return s.impl.Set(ctx, key, value, ttl)
}

// Delete implements Cache.
func (s *Store) Delete(ctx context.Context, keys ...string) error {
	if s == nil || s.impl == nil {
		return errors.New("cache: nil store")
	}
	return s.impl.Delete(ctx, keys...)
}

// Increment implements Cache.
func (s *Store) Increment(ctx context.Context, key string, delta int64) (int64, error) {
	if s == nil || s.impl == nil {
		return 0, errors.New("cache: nil store")
	}
	return s.impl.Increment(ctx, key, delta)
}

func encode(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("cache: marshal value: %w", err)
	}
	return payload, nil
}

func decode(payload []byte, dst any) error {
	if dst == nil {
		return nil
	}
	if err := json.Unmarshal(payload, dst); err != nil {
		return fmt.Errorf("cache: unmarshal value: %w", err)
	}
	return nil
}

func validateTTL(ttl time.Duration) error {
	if ttl < 0 {
		return fmt.Errorf("cache: ttl must be greater than or equal to zero, got %s", ttl)
	}
	return nil
}
