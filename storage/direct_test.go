package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gombit-dev/gombit/storage"
)

// plain is a Storage without direct uploads.
type plain struct{ storage.Storage }

func TestUploadURLNeedsADirectUploader(t *testing.T) {
	ctx := context.Background()
	opts := storage.UploadURLOptions{Expires: time.Minute}
	if _, err := storage.UploadURL(ctx, plain{}, "k", opts); !errors.Is(err, storage.ErrUnsupported) {
		t.Fatalf("UploadURL on a store without direct uploads = %v, want ErrUnsupported", err)
	}
	if _, err := storage.UploadURL(ctx, plain{}, "../k", opts); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("UploadURL with an invalid key = %v, want ErrInvalidKey", err)
	}
}

func TestValidateUploadURLOptions(t *testing.T) {
	if err := storage.ValidateUploadURLOptions(storage.UploadURLOptions{Expires: time.Minute, Size: 0, ContentType: "image/png", Metadata: map[string]string{"filename": "a.png"}}); err != nil {
		t.Fatalf("valid options = %v", err)
	}
	for _, opts := range []storage.UploadURLOptions{
		{},
		{Expires: storage.MaxURLExpiry + time.Second},
		{Expires: time.Minute, Size: -1},
		{Expires: time.Minute, ContentType: "text"},
		{Expires: time.Minute, Metadata: map[string]string{"x": "=?"}},
	} {
		if err := storage.ValidateUploadURLOptions(opts); !errors.Is(err, storage.ErrInvalidOptions) {
			t.Errorf("ValidateUploadURLOptions(%+v) = %v", opts, err)
		}
	}
}
