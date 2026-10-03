package main

import (
	"context"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/framework"
)

// Widget is a sample typed resource for the contract example.
type Widget struct {
	ID    string `json:"id" example:"widget-1" doc:"Stable widget identifier"`
	Name  string `json:"name" example:"First widget" doc:"Human-readable widget name"`
	Color string `json:"color,omitempty" example:"blue" doc:"Optional display color"`
}

// CreateWidgetBody is the typed JSON request body for POST /widgets.
type CreateWidgetBody struct {
	Name  string `json:"name" minLength:"1" maxLength:"80" example:"Second widget" doc:"Human-readable widget name"`
	Color string `json:"color,omitempty" maxLength:"30" example:"green" doc:"Optional display color"`
}

type createWidgetInput struct {
	Body CreateWidgetBody
}

type createWidgetOutput struct {
	Body contract.Data[Widget]
}

type listWidgetsOutput struct {
	Body contract.DataMeta[[]Widget, contract.PageMeta]
}

type getWidgetInput struct {
	ID string `path:"id" doc:"Widget identifier"`
}

type getWidgetOutput struct {
	Body contract.Data[Widget]
}

func registerWidgetRoutes(api huma.API, prefix string) {
	huma.Register(api, huma.Operation{
		OperationID: "list-widgets",
		Method:      http.MethodGet,
		Path:        prefix + "/widgets",
		Summary:     "List widgets",
		Tags:        []string{"Widgets"},
	}, func(ctx context.Context, input *struct{}) (*listWidgetsOutput, error) {
		return &listWidgetsOutput{
			Body: contract.DataMeta[[]Widget, contract.PageMeta]{
				Data: []Widget{
					{ID: "widget-1", Name: "First widget", Color: "blue"},
				},
				Meta: &contract.PageMeta{Page: 1, PerPage: 20, Total: 1},
			},
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-widget",
		Method:      http.MethodGet,
		Path:        prefix + "/widgets/{id}",
		Summary:     "Get a widget",
		Tags:        []string{"Widgets"},
	}, func(ctx context.Context, input *getWidgetInput) (*getWidgetOutput, error) {
		if input.ID != "widget-1" {
			return nil, contract.WithContext(ctx, contract.NotFound("widget not found"))
		}
		return &getWidgetOutput{
			Body: contract.Data[Widget]{
				Data: Widget{ID: "widget-1", Name: "First widget", Color: "blue"},
			},
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-widget",
		Method:      http.MethodPost,
		Path:        prefix + "/widgets",
		Summary:     "Create a widget",
		Tags:        []string{"Widgets"},
	}, func(ctx context.Context, input *createWidgetInput) (*createWidgetOutput, error) {
		return &createWidgetOutput{
			Body: contract.Data[Widget]{
				Data: Widget{
					ID:    "widget-1",
					Name:  input.Body.Name,
					Color: input.Body.Color,
				},
			},
		}, nil
	})
}

func main() {
	app, err := framework.New()
	if err != nil {
		log.Fatal(err)
	}

	registerWidgetRoutes(app.API(), app.Config().API.Prefix)

	if err := framework.Run(app); err != nil {
		log.Fatal(err)
	}
}
