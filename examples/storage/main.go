// Command storage shows object storage end to end: files uploaded to and
// downloaded from App.Storage() through plain HTTP handlers. The store is
// the local driver under ./storage by default (GOMBIT_STORAGE_DRIVER=memory
// keeps files in the process instead); handlers never see which.
//
//	go run ./examples/storage
//	curl -X PUT --data-binary @photo.jpg -H 'Content-Type: image/jpeg' localhost:8080/files/photo
//	curl localhost:8080/files/photo -o copy.jpg
//	curl -X DELETE localhost:8080/files/photo
//	curl -F file=@photo.jpg localhost:8080/uploads
//	curl localhost:8080/uploads/<id> -OJ
//	curl localhost:8080/uploads/<id>/link   # a download link valid for up to 5 minutes
//
// A direct upload sends the bytes straight to storage, not through the app:
//
//	curl -X POST localhost:8080/uploads/direct -d '{"size":1234,"content_type":"image/png","filename":"a.png"}'
//	curl -X PUT --data-binary @a.png -H 'Content-Type: image/png' '<data.upload.url>'
//	curl -X POST localhost:8080/uploads/direct/<id>/confirm
//
// The key is built by the server: "files/" + the id in the path (checked
// first, so a malformed one is a 404, not a server error), or, for a form
// upload, a random id under "uploads/" from storage/upload, which also
// bounds the size, checks the detected type, and keeps the client's
// filename as metadata only.
//
// A link is a signed URL: the browser downloads straight from storage (here,
// the app's own /_storage route; on S3, the bucket) until it expires. Links
// need a signing key, GOMBIT_STORAGE_URL_SECRET (32 bytes or more) or
// GOMBIT_JWT_SECRET, for the local and memory drivers; this example sets a
// development-only one when neither is configured.
//
// internal/document is a generated resource with storage-backed fields (a
// file and an image): ask the field's upload endpoint for a grant, upload,
// then create the record with the key.
//
//	curl -X POST localhost:8080/api/v1/documents/uploads/attachment -d '{"size":1234,"content_type":"application/pdf","filename":"a.pdf"}'
//	curl -X PUT --data-binary @a.pdf -H 'Content-Type: application/pdf' 'localhost:8080<data.upload.url>'
//	curl -X POST localhost:8080/api/v1/documents -d '{"title":"A","attachment":"<data.key>","cover":null}'
//	curl localhost:8080/api/v1/documents
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/examples/storage/internal/document"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/filefield"
	"github.com/gombit-dev/gombit/storage/upload"
)

// maxUpload bounds an upload's size.
const maxUpload = 10 << 20

// devURLSecret signs this example's links when no secret is configured.
// Never use a fixed secret outside development.
const devURLSecret = "dev-only-storage-example-url-signing-secret" // #nosec G101 -- a documented development-only value.

// linkLifetime is how long a download link works, counted from the start
// of the second it is made (storage.SignedURL).
const linkLifetime = 5 * time.Minute

// images is the policy for form uploads: images only, judged by their
// bytes, stored under uploads/ with a generated key.
var images = upload.Policy{
	MaxBytes: maxUpload,
	Types:    []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
	Prefix:   "uploads/",
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.Storage.URLSecret == "" && cfg.Auth.JWTSecret == "" {
		cfg.Storage.URLSecret = devURLSecret
	}
	db, err := database.Open(config.DatabaseConfig{
		Driver: config.DatabaseDriverSQLite,
		DSN:    "file:storage-example?mode=memory&cache=shared&_fk=1",
	})
	if err != nil {
		log.Fatal(err)
	}
	app, err := framework.New(framework.WithConfig(cfg), framework.WithDatabase(db))
	if err != nil {
		log.Fatal(err)
	}
	app.OnStart(func(context.Context) error {
		// A generated app runs `gombit db migrate`; AutoMigrate keeps this
		// example self-contained on an in-memory database.
		return db.AutoMigrate(&document.Document{})
	})
	document.Register(app)
	recs := newRecords(10000)
	register(app.Router(), app.Storage(), recs)
	// Abandoned uploads (stored, never recorded) are swept every hour. A
	// real app runs this as a job, with its database as the records.
	go func() {
		for range time.Tick(time.Hour) {
			if res, err := sweepAbandoned(context.Background(), app.Storage(), recs); err != nil {
				log.Printf("sweep: %v (after %+v)", err, res)
			}
			// The document fields' uploads granted but never attached.
			for _, f := range []struct{ prefix, column string }{{"document/attachment/", "attachment"}, {"document/cover/", "cover"}} {
				if _, err := storage.Sweep(context.Background(), app.Storage(), f.prefix, 2*upload.DefaultGrantExpiry, filefield.ReferencedBy(db.DB, &document.Document{}, f.column)); err != nil {
					log.Printf("sweep %s: %v", f.prefix, err)
				}
			}
		}
	}()
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}

// records stands in for the database table that refers to uploaded
// files: key to filename, up to a fixed number of rows.
type records struct {
	mu   sync.Mutex
	max  int
	rows map[string]string
}

func newRecords(max int) *records { return &records{max: max, rows: map[string]string{}} }

var errFull = errors.New("records: full")

func (r *records) insert(key, filename string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rows) >= r.max {
		return errFull
	}
	r.rows[key] = filename
	return nil
}

func (r *records) remove(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rows[key]
	delete(r.rows, key)
	return ok
}

func (r *records) has(_ context.Context, key string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.rows[key]
	return ok, nil
}

// sweepAbandoned deletes uploads nothing records, once they are older than
// a direct upload grant could still be in use.
func sweepAbandoned(ctx context.Context, store storage.Storage, recs *records) (storage.SweepResult, error) {
	return storage.Sweep(ctx, store, images.Prefix, 2*upload.DefaultGrantExpiry, recs.has)
}

// record writes the record for a stored upload, and deletes the file when
// that fails (storage.DeleteIfFails): no file is left that nothing refers to.
func record(c *gin.Context, store storage.Storage, recs *records, f upload.File) bool {
	err := storage.DeleteIfFails(c.Request.Context(), store, f.Key, func() error { return recs.insert(f.Key, f.Filename()) })
	if errors.Is(err, errFull) {
		fail(c, contract.Conflict("No more files can be stored."))
		return false
	}
	if err != nil {
		fail(c, contract.Internal("could not record the file"))
		return false
	}
	return true
}

// register mounts the file routes on r, backed by store, with the uploads
// recorded in recs.
func register(r gin.IRouter, store storage.Storage, recs *records) {
	// A named file: the body stored as sent, with the client's declared
	// Content-Type (it is served as an attachment, never inline). For files
	// whose type must be checked, see POST /uploads (storage/upload).
	r.PUT("/files/:id", func(c *gin.Context) {
		key, ok := fileKey(c)
		if !ok {
			return
		}
		body := http.MaxBytesReader(c.Writer, c.Request.Body, maxUpload)
		info, err := store.Put(c.Request.Context(), key, body, storage.PutOptions{
			ContentType: c.GetHeader("Content-Type"),
			Size:        storage.SizeFromContentLength(c.Request.ContentLength),
		})
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(c, contract.PayloadTooLarge(""))
			return
		}
		if err != nil {
			fail(c, storage.MapError(c.Request.Context(), err, "file not found", "could not store the file"))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{"key": info.Key, "size": info.Size, "etag": info.ETag}})
	})

	r.GET("/files/:id", func(c *gin.Context) {
		key, ok := fileKey(c)
		if !ok {
			return
		}
		serve(c, store, key, c.Param("id"))
	})

	r.DELETE("/files/:id", func(c *gin.Context) {
		key, ok := fileKey(c)
		if !ok {
			return
		}
		if err := store.Delete(c.Request.Context(), key); err != nil {
			fail(c, storage.MapError(c.Request.Context(), err, "file not found", "could not delete the file"))
			return
		}
		c.Status(http.StatusNoContent)
	})

	// A form upload: multipart/form-data with the image in the "file" field.
	r.POST("/uploads", func(c *gin.Context) {
		f, err := upload.Receive(store, c.Request, images)
		if err != nil {
			fail(c, upload.MapError(c.Request.Context(), err))
			return
		}
		if !record(c, store, recs, f) {
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"id":           strings.TrimPrefix(f.Key, images.Prefix),
			"filename":     f.Filename(),
			"content_type": f.ContentType,
			"size":         f.Size,
		}})
	})

	// A download link. The URL is the authorization: decide who may have
	// it before asking for it (this example lets anyone).
	r.GET("/uploads/:id/link", func(c *gin.Context) {
		key := images.Prefix + c.Param("id")
		if storage.ValidateKey(key) != nil {
			fail(c, contract.NotFound("file not found"))
			return
		}
		u, err := store.URL(c.Request.Context(), key, storage.SignedURL(linkLifetime))
		if errors.Is(err, storage.ErrUnsupported) {
			// No signing key is configured (see the package comment).
			fail(c, contract.Internal("Download links are not configured on this server."))
			return
		}
		if err != nil {
			fail(c, storage.MapError(c.Request.Context(), err, "file not found", "could not make a link"))
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"url": u, "expires_in": int(linkLifetime.Seconds())}})
	})

	// A direct upload, step 1: grant it. The client declares the file;
	// the grant is a signed PUT of exactly that size and type (decide who
	// may upload first; this example lets anyone).
	r.POST("/uploads/direct", func(c *gin.Context) {
		var in struct {
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
			Filename    string `json:"filename"`
		}
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, contract.Validation("Send the file's size, content_type, and filename.", nil))
			return
		}
		g, err := upload.Authorize(c.Request.Context(), store, images, in.Size, in.ContentType, in.Filename)
		if errors.Is(err, storage.ErrUnsupported) {
			fail(c, contract.Internal("Direct uploads are not configured on this server."))
			return
		}
		if err != nil {
			fail(c, upload.MapError(c.Request.Context(), err))
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": strings.TrimPrefix(g.Key, images.Prefix), "upload": g.Request}})
	})

	// Step 3, after the client's PUT: check what arrived, by its bytes.
	// A real app confirms only ids it granted to this caller (kept with
	// the grant), then records the file.
	r.POST("/uploads/direct/:id/confirm", func(c *gin.Context) {
		f, err := upload.Confirm(c.Request.Context(), store, images.Prefix+c.Param("id"), images)
		if err != nil {
			fail(c, upload.MapError(c.Request.Context(), err))
			return
		}
		if !record(c, store, recs, f) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"id":           c.Param("id"),
			"filename":     f.Filename(),
			"content_type": f.ContentType,
			"size":         f.Size,
		}})
	})

	// Deleting an upload: the record first, then the file, which the
	// record owns (its key is under images.Prefix, generated for it).
	r.DELETE("/uploads/:id", func(c *gin.Context) {
		key := images.Prefix + c.Param("id")
		if storage.ValidateKey(key) != nil || !recs.remove(key) {
			fail(c, contract.NotFound("file not found"))
			return
		}
		if _, err := storage.DeleteOwned(c.Request.Context(), store, key, images.Prefix); err != nil {
			// The record is gone; the file is now abandoned, and the
			// sweep removes it.
			log.Printf("delete %s: %v", key, err)
		}
		c.Status(http.StatusNoContent)
	})

	r.GET("/uploads/:id", func(c *gin.Context) {
		key := images.Prefix + c.Param("id")
		if storage.ValidateKey(key) != nil {
			fail(c, contract.NotFound("file not found"))
			return
		}
		serve(c, store, key, "")
	})
}

// serve writes the object at key as a download named filename (or the
// name it was uploaded with, when filename is empty).
func serve(c *gin.Context, store storage.Storage, key, filename string) {
	body, info, err := store.Open(c.Request.Context(), key)
	if err != nil {
		fail(c, storage.MapError(c.Request.Context(), err, "file not found", "could not read the file"))
		return
	}
	defer func() { _ = body.Close() }()
	if filename == "" {
		filename = info.Metadata[upload.FilenameMetadata]
	}
	// An attachment, never rendered inline: the content type came from
	// the uploader, and a text/html or image/svg+xml object rendered as a
	// same-origin page would run in the app's origin.
	disposition := "attachment"
	if filename != "" {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	}
	c.Header("Content-Disposition", disposition)
	c.Header("Content-Type", info.ContentType)
	c.Header("Content-Length", strconv.FormatInt(info.Size, 10))
	c.Header("ETag", strconv.Quote(info.ETag))
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

// fileKey builds the object key from the path id, answering 404 for an id
// that cannot form a valid key.
func fileKey(c *gin.Context) (string, bool) {
	key := "files/" + c.Param("id")
	if storage.ValidateKey(key) != nil {
		fail(c, contract.NotFound("file not found"))
		return "", false
	}
	return key, true
}

func fail(c *gin.Context, err error) {
	var env *contract.ErrorEnvelope
	if !errors.As(err, &env) {
		env = contract.Internal("unexpected error")
	}
	c.AbortWithStatusJSON(env.GetStatus(), env)
}
