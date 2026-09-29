// Command storage shows object storage end to end: files uploaded to and
// downloaded from App.Storage() through plain HTTP handlers. The store is
// the local driver under ./storage by default (GOMBIT_STORAGE_DRIVER=memory
// keeps files in the process instead); handlers never see which.
//
//	go run ./examples/storage
//	curl -X PUT --data-binary @photo.jpg -H 'Content-Type: image/jpeg' localhost:8080/files/photo
//	curl localhost:8080/files/photo -o copy.jpg
//	curl -X DELETE localhost:8080/files/photo
//
// The key is built by the server ("files/" + the id in the path); the id is
// checked first, so a malformed one is a 404, not a server error.
package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
	"github.com/gombit-dev/gombit/storage"
)

// maxUpload bounds an upload's size.
const maxUpload = 10 << 20

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
		body, info, err := store.Open(c.Request.Context(), key)
		if err != nil {
			fail(c, storage.MapError(c.Request.Context(), err, "file not found", "could not read the file"))
			return
		}
		defer func() { _ = body.Close() }()
		c.Header("Content-Type", info.ContentType)
		c.Header("Content-Length", strconv.FormatInt(info.Size, 10))
		c.Header("ETag", strconv.Quote(info.ETag))
		c.Status(http.StatusOK)
		_, _ = io.Copy(c.Writer, body)
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
