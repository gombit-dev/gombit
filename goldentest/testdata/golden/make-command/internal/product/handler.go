package product

import (
	"context"
	"strconv"

	"github.com/gombit-dev/gombit/contract"
	"github.com/gombit-dev/gombit/database"
	"gorm.io/gorm"
)

// Handler serves product HTTP operations over GORM.
type Handler struct {
	DB *gorm.DB
}

type productData struct {
	ID    uint   `json:"id" example:"1" doc:"Product identifier"`
	Name  string `json:"name" example:"Notebook" doc:"Human-readable product name"`
	Price int64  `json:"price" example:"1299" doc:"Price in the smallest currency unit"`
}

type listProductsOutput struct {
	Body contract.DataMeta[[]productData, contract.PageMeta]
}

type listProductsInput struct {
	Page    int `query:"page" doc:"1-based page"`
	PerPage int `query:"per_page" doc:"Page size"`
}

type getProductInput struct {
	ID string `path:"id" doc:"Product identifier"`
}

type getProductOutput struct {
	Body contract.Data[productData]
}

type createProductInput struct {
	Body struct {
		Name  string `json:"name" minLength:"1" maxLength:"120" example:"Notebook" doc:"Human-readable product name"`
		Price int64  `json:"price" minimum:"0" example:"1299" doc:"Price in the smallest currency unit"`
	}
}

type createProductOutput struct {
	Body contract.Data[productData]
}

func (h *Handler) list(ctx context.Context, input *listProductsInput) (*listProductsOutput, error) {
	page, perPage := contract.ClampPage(input.Page, input.PerPage)
	q := h.DB.WithContext(ctx).Model(&Product{})
	var total int64
	if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("list products"))
	}
	var rows []Product
	if err := q.Order("id").Offset(contract.PageOffset(page, perPage)).Limit(perPage).Find(&rows).Error; err != nil {
		return nil, contract.WithContext(ctx, contract.Internal("list products"))
	}
	items := make([]productData, 0, len(rows))
	for _, row := range rows {
		items = append(items, toProductData(row))
	}
	return &listProductsOutput{
		Body: contract.DataMeta[[]productData, contract.PageMeta]{
			Data: items,
			Meta: &contract.PageMeta{Page: page, PerPage: perPage, Total: total},
		},
	}, nil
}

func (h *Handler) get(ctx context.Context, input *getProductInput) (*getProductOutput, error) {
	// 63 bits: an id over MaxInt64 is no row's, and PostgreSQL's bigint
	// cannot even bind it, so it is a 404 with the other bad ids.
	id, err := strconv.ParseUint(input.ID, 10, 63)
	if err != nil {
		return nil, contract.WithContext(ctx, contract.NotFound("product not found"))
	}
	var row Product
	if err := h.DB.WithContext(ctx).First(&row, uint(id)).Error; err != nil {
		return nil, database.MapLoadError(ctx, err, "product not found", "load product")
	}
	return &getProductOutput{
		Body: contract.Data[productData]{Data: toProductData(row)},
	}, nil
}

func (h *Handler) create(ctx context.Context, input *createProductInput) (*createProductOutput, error) {
	row := Product{Name: input.Body.Name, Price: input.Body.Price}
	if err := h.DB.WithContext(ctx).Create(&row).Error; err != nil {
		return nil, database.MapPersistError(ctx, err, "resource already exists", "create product")
	}
	return &createProductOutput{
		Body: contract.Data[productData]{Data: toProductData(row)},
	}, nil
}

func toProductData(row Product) productData {
	return productData{ID: row.ID, Name: row.Name, Price: row.Price}
}
