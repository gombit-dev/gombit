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
//	curl localhost:8080/uploads/<id>/link   # a download link valid for 5 minutes
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
package main

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
	"github.com/gombit-dev/gombit/storage/upload"
)

// maxUpload bounds an upload's size.
const maxUpload = 10 << 20

// linkLifetime is how long a download link works.
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
	app, err := framework.New(framework.WithConfig(cfg))
	if err != nil {
		log.Fatal(err)
	}
	register(app.Router(), app.Storage())
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}

// register mounts the file routes on r, backed by store.
func register(r gin.IRouter, store storage.Storage) {
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
