package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
)

//go:embed all:static
var files embed.FS

type pingBody struct {
	Status string `json:"status"`
}

type pingOutput struct {
	Body contract.Data[pingBody]
}

func main() {
	cfg := config.Default()
	cfg.HTTP.Addr = "127.0.0.1:8080"

	sub, err := fs.Sub(files, "static")
	if err != nil {
		log.Fatal(err)
	}

	app, err := framework.New(
		framework.WithConfig(cfg),
		framework.WithEmbeddedFrontend(sub),
	)
	if err != nil {
		log.Fatal(err)
	}

	huma.Register(app.API(), huma.Operation{
		OperationID: "embed-ping",
		Method:      http.MethodGet,
		Path:        cfg.API.Prefix + "/ping",
		Summary:     "Ping",
	}, func(ctx context.Context, input *struct{}) (*pingOutput, error) {
		return &pingOutput{Body: contract.Data[pingBody]{Data: pingBody{Status: "ok"}}}, nil
	})

	log.Printf("embed example listening on http://%s (docs http://127.0.0.1:8080/docs)", cfg.HTTP.Addr)
	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}
