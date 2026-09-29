package storage_test

import (
	"context"
	"errors"
	"go/build"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/storage"
)

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"avatars/42.png",
		"one/two/three/four.txt",
		"spaces in name/file (1).pdf",
		"ünïcødé/数据.bin",
		"dots/..hidden/.also/name..ext",
		"c:/drive-like/segment",
		strings.Repeat("k", storage.MaxKeyBytes),
	}
	for _, key := range valid {
		if err := storage.ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := map[string]string{
		"":               "empty",
		"/abs":           "starts with '/'",
		"trailing/":      "ends with '/'",
		"a//b":           "empty segment",
		".":              `"." segment`,
		"..":             `".." segment`,
		"../escape":      `".." segment`,
		"a/../../escape": `".." segment`,
		"a/./b":          `"." segment`,
		`a\b`:            "backslash",
		"nul\x00byte":    "control character",
		"new\nline":      "control character",
		"del\x7fchar":    "control character",
		"bad\xffutf8":    "UTF-8",
		strings.Repeat("k", storage.MaxKeyBytes+1): "longer than",
	}
	for key, reason := range invalid {
		err := storage.ValidateKey(key)
		if !errors.Is(err, storage.ErrInvalidKey) || !strings.Contains(err.Error(), reason) {
			t.Errorf("ValidateKey(%q) = %v, want storage.ErrInvalidKey mentioning %q", key, err, reason)
		}
	}
}

func TestValidatePutOptions(t *testing.T) {
	ok := []storage.PutOptions{
		{},
		{ContentType: "image/png", Size: 10},
		{ContentType: "text/plain; charset=utf-8"},
		{Metadata: map[string]string{"original-name": "résumé final.pdf", "v2": ""}},
		{Metadata: map[string]string{"n": strings.Repeat("v", storage.MaxMetadataBytes-1)}},
	}
	for _, opts := range ok {
		if err := storage.ValidatePutOptions(opts); err != nil {
			t.Errorf("ValidatePutOptions(%+v) = %v, want nil", opts, err)
		}
	}
	bad := []storage.PutOptions{
		{ContentType: "not a media type"},
		{ContentType: "text/"},
		{Size: -1},
		{Metadata: map[string]string{"": "x"}},
		{Metadata: map[string]string{"Upper": "x"}},
		{Metadata: map[string]string{"under_score": "x"}},
		{Metadata: map[string]string{"ok": "tab\there"}},
		{Metadata: map[string]string{"ok": "bad\xffutf8"}},
		{Metadata: map[string]string{"n": strings.Repeat("v", storage.MaxMetadataBytes)}},
	}
	for _, opts := range bad {
		if err := storage.ValidatePutOptions(opts); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidatePutOptions(%+v) = %v, want storage.ErrInvalidOptions", opts, err)
		}
	}
	if err := storage.ValidateURLOptions(storage.SignedURL(time.Minute)); err != nil {
		t.Errorf("ValidateURLOptions(SignedURL) = %v", err)
	}
	if err := storage.ValidateURLOptions(storage.PublicURL()); err != nil {
		t.Errorf("ValidateURLOptions(PublicURL) = %v", err)
	}
	if err := storage.ValidateURLOptions(storage.URLOptions{Expires: -1}); !errors.Is(err, storage.ErrInvalidOptions) {
		t.Errorf("ValidateURLOptions(negative) = %v, want storage.ErrInvalidOptions", err)
	}
}

func TestExpectSize(t *testing.T) {
	read := func(r io.Reader) (string, error) {
		b, err := io.ReadAll(r)
		return string(b), err
	}
	if got, err := read(storage.ExpectSize(strings.NewReader("12345"), 5)); err != nil || got != "12345" {
		t.Errorf("exact = %q, %v", got, err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("123"), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("short = %v, want storage.ErrSizeMismatch", err)
	}
	if _, err := read(storage.ExpectSize(strings.NewReader("1234567"), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("long = %v, want storage.ErrSizeMismatch", err)
	}
	// One byte at a time, and a reader that returns data with EOF.
	if got, err := read(storage.ExpectSize(iotest.OneByteReader(strings.NewReader("12345")), 5)); err != nil || got != "12345" {
		t.Errorf("one byte at a time = %q, %v", got, err)
	}
	if got, err := read(storage.ExpectSize(iotest.DataErrReader(strings.NewReader("12345")), 5)); err != nil || got != "12345" {
		t.Errorf("data with EOF = %q, %v", got, err)
	}
	if _, err := read(storage.ExpectSize(iotest.DataErrReader(strings.NewReader("123456")), 5)); !errors.Is(err, storage.ErrSizeMismatch) {
		t.Errorf("long, data with EOF = %v, want storage.ErrSizeMismatch", err)
	}
	// A source error passes through unchanged.
	boom := errors.New("boom")
	if _, err := read(storage.ExpectSize(iotest.ErrReader(boom), 5)); !errors.Is(err, boom) {
		t.Errorf("source error = %v, want it passed through", err)
	}
	// Zero means unknown: no limit.
	r := strings.NewReader("anything")
	if storage.ExpectSize(r, 0) != io.Reader(r) {
		t.Error("ExpectSize(r, 0) should return r itself")
	}
}

func TestErrorAndWrap(t *testing.T) {
	err := storage.Wrap("open", "a/b", storage.ErrNotFound)
	if !errors.Is(err, storage.ErrNotFound) || err.Error() != `storage: open "a/b": storage: object not found` {
		t.Errorf("Wrap = %v", err)
	}
	var se *storage.Error
	if !errors.As(err, &se) || se.Op != "open" || se.Key != "a/b" {
		t.Errorf("errors.As = %+v", se)
	}
	if again := storage.Wrap("put", "x", err); again != err {
		t.Errorf("Wrap of an *Error rewrapped it: %v", again)
	}
	if storage.Wrap("put", "x", nil) != nil {
		t.Error("Wrap(nil) != nil")
	}
}

func TestMapError(t *testing.T) {
	ctx := context.Background()
	cause := errors.New("s3: SlowDown: dial tcp 10.0.0.1:443: i/o timeout")
	cases := []struct {
		err    error
		status int
	}{
		{storage.Wrap("open", "k", storage.ErrNotFound), http.StatusNotFound},
		{storage.Wrap("put", "k", storage.ErrInvalidKey), http.StatusUnprocessableEntity},
		{storage.Wrap("put", "k", storage.ErrInvalidOptions), http.StatusUnprocessableEntity},
		{storage.Wrap("put", "k", storage.ErrSizeMismatch), http.StatusUnprocessableEntity},
		{storage.Wrap("put", "k", errors.Join(storage.ErrUnavailable, cause)), http.StatusServiceUnavailable},
		{storage.Wrap("url", "k", storage.ErrUnsupported), http.StatusInternalServerError},
		{cause, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		mapped := storage.MapError(ctx, tc.err, "file not found", "could not read the file")
		var env *contract.ErrorEnvelope
		if !errors.As(mapped, &env) {
			t.Fatalf("MapError(%v) = %T, want *contract.ErrorEnvelope", tc.err, mapped)
		}
		if env.GetStatus() != tc.status {
			t.Errorf("MapError(%v) status = %d, want %d", tc.err, env.GetStatus(), tc.status)
		}
		if strings.Contains(env.Body.Message, "10.0.0.1") {
			t.Errorf("MapError leaked the driver's error text: %q", env.Body.Message)
		}
	}
	if storage.MapError(ctx, nil, "", "") != nil {
		t.Error("MapError(nil) != nil")
	}
}

// stubStore answers Stat with a fixed error.
type stubStore struct {
	storage.Storage
	statErr error
}

func (s stubStore) Stat(context.Context, string) (storage.ObjectInfo, error) {
	return storage.ObjectInfo{}, s.statErr
}

func TestExists(t *testing.T) {
	ctx := context.Background()
	if ok, err := storage.Exists(ctx, stubStore{}, "k"); !ok || err != nil {
		t.Errorf("present = %v, %v", ok, err)
	}
	if ok, err := storage.Exists(ctx, stubStore{statErr: storage.Wrap("stat", "k", storage.ErrNotFound)}, "k"); ok || err != nil {
		t.Errorf("missing = %v, %v; want false, nil", ok, err)
	}
	boom := storage.Wrap("stat", "k", storage.ErrUnavailable)
	if ok, err := storage.Exists(ctx, stubStore{statErr: boom}, "k"); ok || !errors.Is(err, storage.ErrUnavailable) {
		t.Errorf("failing = %v, %v; want false and the error", ok, err)
	}
}

// TestNoDriverDependencies: the contract package, which application code
// imports, depends on nothing but the standard library and Gombit's own
// contract package, so it can never pull an S3 SDK or a filesystem driver
// into an application.
func TestNoDriverDependencies(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if imp == "github.com/gombit-dev/gombit/contract" || !strings.Contains(strings.Split(imp, "/")[0], ".") {
			continue
		}
		t.Errorf("package storage imports %s: the contract must not depend on a driver or third-party module", imp)
	}
}
