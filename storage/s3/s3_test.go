package s3

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/storage"
)

func TestMetadataRoundTrips(t *testing.T) {
	md := map[string]string{
		"original-name": "résumé final (1).pdf",
		"percent":       "100% sure %20 not a space",
		"plain":         "ascii only",
		"empty":         "",
	}
	enc := encodeMetadata(md)
	for name, value := range enc {
		for i := 0; i < len(value); i++ {
			if c := value[i]; c < 0x20 || c > 0x7e {
				t.Fatalf("encoded %q = %q is not printable ASCII", name, value)
			}
		}
	}
	if enc["plain"] != "ascii only" || enc["percent"] != md["percent"] {
		t.Fatalf("printable ASCII was encoded: %q, %q", enc["plain"], enc["percent"])
	}
	upper := map[string]string{}
	for k, v := range enc {
		upper[strings.ToUpper(k[:1])+k[1:]] = v // S3 may return names with other casing
	}
	dec, err := decodeMetadata(upper)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range md {
		if dec[k] != v {
			t.Fatalf("%s round-tripped to %q, want %q", k, dec[k], v)
		}
	}
	// What S3 returns for a non-ASCII value it stored: its own encoding.
	if dec, err := decodeMetadata(map[string]string{"n": "=?UTF-8?Q?r=C3=A9sum=C3=A9?="}); err != nil || dec["n"] != "résumé" {
		t.Fatalf("S3's Q-encoding decoded to %q, %v", dec["n"], err)
	}
	if encodeMetadata(nil) != nil {
		t.Fatal("encodeMetadata(nil) != nil")
	}
}

func TestObjectKeyAppliesThePrefix(t *testing.T) {
	s := &Store{prefix: "app/prod/"}
	if got, err := s.objectKey("avatars/1.png"); err != nil || got != "app/prod/avatars/1.png" {
		t.Fatalf("objectKey = %q, %v", got, err)
	}
	if _, err := s.objectKey("../x"); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("objectKey(../x) = %v", err)
	}
	long := strings.Repeat("k", storage.MaxKeyBytes-len(s.prefix)+1)
	if _, err := s.objectKey(long); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("a key over S3's limit with the prefix = %v, want storage.ErrInvalidKey", err)
	}
}

func responseError(status int) error {
	return &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}}, Err: errors.New("api error")}
}

func TestClassify(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"no such key":   {&types.NoSuchKey{}, storage.ErrNotFound},
		"head 404":      {&types.NotFound{}, storage.ErrNotFound},
		"status 404":    {responseError(404), storage.ErrNotFound},
		"throttled":     {responseError(429), storage.ErrUnavailable},
		"server error":  {responseError(503), storage.ErrUnavailable},
		"network":       {&net.OpError{Op: "dial", Err: errors.New("connection refused")}, storage.ErrUnavailable},
		"size mismatch": {storage.ErrSizeMismatch, storage.ErrSizeMismatch},
	} {
		if got := classify(ctx, tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%s: classify = %v, want %v", name, got, tc.want)
		}
	}
	denied := responseError(403)
	if got := classify(ctx, denied); errors.Is(got, storage.ErrNotFound) || errors.Is(got, storage.ErrUnavailable) {
		t.Errorf("access denied classified as %v; it is a configuration error", got)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got := classify(canceled, errors.Join(errors.New("operation error"), context.Canceled)); got != context.Canceled {
		t.Errorf("canceled = %v, want context.Canceled", got)
	}
	if classify(ctx, nil) != nil {
		t.Error("classify(nil) != nil")
	}
}

func TestNewValidates(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{
		{Region: "us-east-1"},
		{Bucket: "b"},
		{Bucket: "b", Region: "us-east-1", AccessKeyID: "only-the-id"},
		{Bucket: "b", Region: "us-east-1", Prefix: "myapp"},      // no trailing '/'
		{Bucket: "b", Region: "us-east-1", Prefix: "../escape/"}, // not a valid key path
		{Bucket: "b", Region: "us-east-1", Prefix: "app./prod/"}, // a segment ending with '.'
	} {
		if _, err := New(ctx, cfg); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	cfg := Config{Bucket: "b", Region: "auto", Endpoint: "http://127.0.0.1:9", AccessKeyID: "id", SecretAccessKey: "top-secret"}
	if _, err := New(ctx, cfg); err != nil {
		t.Fatalf("New = %v", err)
	}
	if s := cfg.String(); strings.Contains(s, "top-secret") || strings.Contains(s, "id\"") {
		t.Fatalf("Config.String() leaks credentials: %s", s)
	}
}

// TestEncodingMatchesTheContractsMeasure: the driver sends each value
// exactly as long as storage.MetadataValueWireLen says, so metadata that
// storage.ValidateMetadata accepts fits S3's 2 KB as sent.
func TestEncodingMatchesTheContractsMeasure(t *testing.T) {
	for _, v := range []string{"", "ascii", "100%", "é", strings.Repeat("é", 700), "tab\there"} {
		enc := encodeMetadata(map[string]string{"n": v})["n"]
		if len(enc) != storage.MetadataValueWireLen(v) {
			t.Errorf("%q encodes to %d bytes; the contract counts %d", v, len(enc), storage.MetadataValueWireLen(v))
		}
	}
}

// TestPrefixRuleIsShared: config validation and New accept exactly the
// same prefixes, in both directions: one rule (storage.ValidatePrefix).
// A prefix is a valid key followed by '/' that leaves room for a key.
func TestPrefixRuleIsShared(t *testing.T) {
	ctx := context.Background()
	for prefix, valid := range map[string]bool{
		"":                                true,
		"myapp/":                          true,
		"a/b/c/":                          true,
		"v1..beta/":                       true, // ".." inside a segment is fine
		"café/":                           true,
		strings.Repeat("s", 255) + "/":    true,
		strings.Repeat("a/", 510) + "bc/": true,  // 1023 bytes: one byte left for a key
		"myapp":                           false, // no trailing '/'
		"/myapp/":                         false,
		"a//b/":                           false,
		"../escape/":                      false,
		"app./prod/":                      false, // a segment ending with '.'
		"app /":                           false, // ... or a space
		"a\x01b/":                         false, // a control character
		"a\u202eb/":                       false, // a bidirectional control
		`a\b/`:                            false,
		strings.Repeat("s", 256) + "/":    false, // a segment too long
		strings.Repeat("a/", 512):         false, // 1024 bytes: no room for any key
		"\xff/":                           false,
	} {
		_, newErr := New(ctx, Config{Bucket: "b", Region: "us-east-1", Prefix: prefix})
		cfgErr := config.ValidateStorage(config.StorageConfig{Driver: config.StorageDriverS3, S3: config.S3StorageConfig{Bucket: "b", Region: "us-east-1", Prefix: prefix}})
		if (newErr == nil) != valid || (cfgErr == nil) != valid {
			t.Errorf("prefix %.40q: New = %v, config = %v; want valid = %v", prefix, newErr, cfgErr, valid)
		}
		if newErr != nil && !errors.Is(newErr, storage.ErrInvalidKey) {
			t.Errorf("prefix %.40q: New = %v, want ErrInvalidKey", prefix, newErr)
		}
	}
}
