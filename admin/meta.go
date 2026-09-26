package admin

import (
	"context"

	"github.com/gombit-dev/gombit/auth"
	"github.com/gombit-dev/gombit/contract"
)

// Catalog is the GET /admin/meta success data object.
type Catalog struct {
	Models []ModelMeta `json:"models"`
}

// CatalogAux is optional envelope meta on the catalog.
type CatalogAux struct {
	Auth *AuthMeta `json:"auth,omitempty"`
}

// AuthMeta tells the SPA which authorization rule guards the catalog.
type AuthMeta struct {
	Mode      string `json:"mode"`
	Bootstrap string `json:"bootstrap"`
}

// ModelMeta is one registered model in the introspection API.
type ModelMeta struct {
	Slug        string       `json:"slug"`
	Singular    string       `json:"singular"`
	Plural      string       `json:"plural"`
	PK          string       `json:"pk"`
	Fields      []FieldMeta  `json:"fields"`
	List        []string     `json:"list"`
	Search      []string     `json:"search"`
	Filter      []string     `json:"filter"`
	Ordering    []string     `json:"ordering"`
	Actions     Actions      `json:"actions"`
	Permissions Permissions  `json:"permissions"`
	Can         Capabilities `json:"can"`
}

// Capabilities are the current user's enabled actions for one model.
type Capabilities struct {
	View   bool `json:"view"`
	Create bool `json:"create"`
	Update bool `json:"update"`
	Delete bool `json:"delete"`
}

// FieldMeta is the introspection shape of a field (no Column).
type FieldMeta struct {
	Name      string    `json:"name"`
	Type      FieldType `json:"type"`
	Required  bool      `json:"required"`
	ReadOnly  bool      `json:"readonly"`
	Related   *Relation `json:"related,omitempty"`
	Minimum   string    `json:"minimum,omitempty"`
	Maximum   string    `json:"maximum,omitempty"`
	MaxLength int       `json:"max_length,omitempty"`
	Pattern   string    `json:"pattern,omitempty"`
	Default   string    `json:"default,omitempty"`
	Format    string    `json:"format,omitempty"`
	Choices   []Choice  `json:"choices,omitempty"`
}

type catalogOutput struct {
	Body contract.DataMeta[Catalog, CatalogAux]
}

type modelOutput struct {
	Body contract.Data[ModelMeta]
}

type slugInput struct {
	Slug string `path:"slug" doc:"Registered model slug"`
}

func modelMetaFrom(opts Options, pk string) ModelMeta {
	fields := make([]FieldMeta, 0, len(opts.Fields))
	for _, f := range opts.Fields {
		var rel *Relation
		if f.Related != nil {
			copyRel := *f.Related
			rel = &copyRel
		}
		fields = append(fields, FieldMeta{
			Name:      f.Name,
			Type:      f.Type,
			Required:  f.Required,
			ReadOnly:  f.ReadOnly || (f.Type == TypeRelation && f.Related != nil && f.Related.Kind == RelHasMany),
			Related:   rel,
			Minimum:   f.Minimum,
			Maximum:   f.Maximum,
			MaxLength: f.MaxLength,
			Pattern:   f.Pattern,
			Default:   f.Default,
			Format:    f.Format,
			Choices:   cloneChoices(f.Choices),
		})
	}
	return ModelMeta{
		Slug:        opts.Slug,
		Singular:    opts.Singular,
		Plural:      opts.Plural,
		PK:          pk,
		Fields:      fields,
		List:        cloneStrings(opts.List),
		Search:      cloneStrings(opts.Search),
		Filter:      cloneStrings(opts.Filter),
		Ordering:    cloneStrings(opts.Ordering),
		Actions:     opts.Actions,
		Permissions: opts.Permissions,
	}
}

func cloneStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneChoices(in []Choice) []Choice {
	if len(in) == 0 {
		return nil
	}
	out := make([]Choice, len(in))
	copy(out, in)
	return out
}

func (h *handlers) listMeta(ctx context.Context, _ *struct{}) (*catalogOutput, error) {
	models := h.reg.all()
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return nil, contract.WithContext(ctx, contract.Authentication("missing session cookie"))
	}
	grants, err := h.permissionGrants(ctx, permissionKeys(models)...)
	if err != nil {
		return nil, err
	}
	catalog := Catalog{Models: make([]ModelMeta, 0, len(models))}
	for _, m := range models {
		if !grants[m.meta.Permissions.View] {
			continue
		}
		meta := m.meta
		meta.Can = capabilities(m, grants)
		catalog.Models = append(catalog.Models, meta)
	}
	if len(catalog.Models) == 0 && !user.IsSuperuser {
		return nil, contract.WithContext(ctx, contract.Authorization("no admin view permissions"))
	}
	return &catalogOutput{
		Body: contract.DataMeta[Catalog, CatalogAux]{
			Data: catalog,
			Meta: &CatalogAux{Auth: &AuthMeta{Mode: "cookie", Bootstrap: "permissions"}},
		},
	}, nil
}

func (h *handlers) getMeta(ctx context.Context, input *slugInput) (*modelOutput, error) {
	m, ok := h.reg.get(input.Slug)
	if !ok {
		return nil, contract.WithContext(ctx, contract.NotFound("unknown model"))
	}
	grants, err := h.permissionGrants(ctx,
		m.meta.Permissions.View,
		m.meta.Permissions.Create,
		m.meta.Permissions.Update,
		m.meta.Permissions.Delete,
	)
	if err != nil {
		return nil, err
	}
	if !grants[m.meta.Permissions.View] {
		return nil, contract.WithContext(ctx, contract.Authorization("admin permission denied"))
	}
	meta := m.meta
	meta.Can = capabilities(m, grants)
	return &modelOutput{Body: contract.Data[ModelMeta]{Data: meta}}, nil
}
