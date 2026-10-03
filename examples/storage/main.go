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
// A direct upload is a signed PUT the client makes on its own, with the
// grant the app gave it. Only with S3 (GOMBIT_STORAGE_DRIVER=s3) do the
// bytes go straight to the bucket, bypassing the app process; with the
// local driver (the default) and memory, the PUT goes to the app's own
// storage route (/_storage), which streams the bytes into the store, so
// they pass through the app process (though not through these handlers):
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
// GOMBIT_JWT_SECRET, for the local and memory drivers.
//
// Uploads are recorded in a SQLite database (storage-example.db) under the
// ownership protocol of storage/claims: every upload's key is claimed
// (pending) before anything is stored; the record that refers to it holds
// it in the same transaction; deleting the record releases it; and an hourly
// sweep deletes uploads no record ever held. So no cleanup ever deletes a
// file a record refers to, whatever fails or races.
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
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/claims"
	"github.com/gombit-dev/gombit/storage/upload"
)

// maxUpload bounds an upload's size.
const maxUpload = 10 << 20

// linkLifetime is how long a download link works, counted from the start
// of the second it is made (storage.SignedURL).
const linkLifetime = 5 * time.Minute

// images is the policy for uploads: images only, judged by their bytes,
// stored under uploads/ with a generated key (files.policy adds the claim).
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
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	db, err := database.Open(config.DatabaseConfig{Driver: config.DatabaseDriverSQLite, DSN: "file:storage-example.db?_fk=1&_busy_timeout=5000"})
	if err != nil {
		log.Fatal(err)
	}
	fs, err := newFiles(db.DB, app.Storage(), 10000)
	if err != nil {
		log.Fatal(err)
	}
	register(app.Router(), fs)
	// Uploads no record ever held (abandoned forms, unconfirmed direct
	// uploads) are swept every hour, once older than a grant and its
	// confirmation could take. A real app runs this as a job.
	go func() {
		for range time.Tick(time.Hour) {
			if res, err := fs.claims.Sweep(context.Background(), 2*upload.DefaultGrantExpiry); err != nil {
				log.Printf("sweep: %v (after %+v)", err, res)
			}
			// Staged direct uploads left over (a late S3 PUT, a promoted
			// upload's staged copy). Only S3 needs it: the app's own route
			// ends local and memory uploads within their claims' leases.
			if cfg.Storage.Driver == config.StorageDriverS3 {
				if _, err := fs.claims.SweepStaging(context.Background()); err != nil {
					log.Printf("sweep staging: %v", err)
				}
			}
		}
	}()
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}

// fileRecord is the table that refers to uploaded files: one record per
// file, by key.
type fileRecord struct {
	Key      string `gorm:"primaryKey;size:512"`
	Filename string `gorm:"size:255"`
}

// files is the upload side of the example: the store, the records, and the
// claims that tie them together.
type files struct {
	store  storage.Storage
	db     *gorm.DB
	claims *claims.Claims
	max    int // the most records (to show an insert failing)
}

func newFiles(db *gorm.DB, store storage.Storage, max int) (*files, error) {
	if err := db.AutoMigrate(append(claims.Models(), &fileRecord{})...); err != nil {
		return nil, err
	}
	return &files{
		store:  store,
		db:     db,
		claims: claims.New(db, store, claims.WithWarn(func(msg string, err error) { log.Printf("%s: %v", msg, err) })),
		max:    max,
	}, nil
}

// policy is images with every upload's key claimed before it is stored.
func (fs *files) policy() upload.Policy {
	p := images
	p.Claims = fs.claims
	return p
}

var errFull = errors.New("records: full")

// record writes the record for a stored upload, holding its key in the
// same transaction (claims.CreateWith). If that fails, the file is deleted,
// unless the key turns out held after all (a commit whose answer was lost,
// a retried confirmation): no file is left that nothing refers to, and no
// file a record refers to is ever deleted.
func (fs *files) record(c *gin.Context, f upload.File) bool {
	err := fs.claims.CreateWith(c.Request.Context(), []string{f.Key}, func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&fileRecord{}).Count(&n).Error; err != nil {
			return err
		}
		if n >= int64(fs.max) {
			return errFull
		}
		return tx.Create(&fileRecord{Key: f.Key, Filename: f.Filename}).Error
	})
	switch {
	case errors.Is(err, errFull):
		fail(c, contract.Conflict("No more files can be stored."))
		return false
	case errors.Is(err, claims.ErrNotPending):
		fail(c, contract.Conflict("This file is already recorded, or was cleaned up."))
		return false
	case err != nil:
		fail(c, contract.Internal("could not record the file"))
		return false
	}
	return true
}

// register mounts the file routes on r, backed by fs.
func register(r gin.IRouter, fs *files) {
	store := fs.store
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
		f, err := upload.Receive(store, c.Request, fs.policy())
		if err != nil {
			fail(c, upload.MapError(c.Request.Context(), err))
			return
		}
		if !fs.record(c, f) {
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{
			"id":           strings.TrimPrefix(f.Key, images.Prefix),
			"filename":     f.Filename,
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
		g, err := upload.Authorize(c.Request.Context(), store, fs.policy(), in.Size, in.ContentType, in.Filename)
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
		f, err := upload.Confirm(c.Request.Context(), store, images.Prefix+c.Param("id"), fs.policy())
		if err != nil {
			fail(c, upload.MapError(c.Request.Context(), err))
			return
		}
		if !fs.record(c, f) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"id":           c.Param("id"),
			"filename":     f.Filename,
			"content_type": f.ContentType,
			"size":         f.Size,
		}})
	})

	// Deleting an upload: the record, with its key released in the same
	// transaction, then the file (claims.DeleteWith). If deleting the file
	// fails, the record is still gone, and the sweep finishes the delete.
	r.DELETE("/uploads/:id", func(c *gin.Context) {
		key := images.Prefix + c.Param("id")
		if storage.ValidateKey(key) != nil {
			fail(c, contract.NotFound("file not found"))
			return
		}
		err := fs.claims.DeleteWith(c.Request.Context(), []string{key}, func(tx *gorm.DB) error {
			res := tx.Delete(&fileRecord{Key: key})
			if res.Error == nil && res.RowsAffected == 0 {
				return errNoRecord
			}
			return res.Error
		})
		switch {
		case errors.Is(err, errNoRecord):
			fail(c, contract.NotFound("file not found"))
		case err != nil:
			fail(c, contract.Internal("could not delete the file"))
		default:
			c.Status(http.StatusNoContent)
		}
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

var errNoRecord = errors.New("records: no such file")

func fail(c *gin.Context, err error) {
	var env *contract.ErrorEnvelope
	if !errors.As(err, &env) {
		env = contract.Internal("unexpected error")
	}
	c.AbortWithStatusJSON(env.GetStatus(), env)
}
