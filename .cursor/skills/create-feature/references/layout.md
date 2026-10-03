# Generated application layout

Source of truth: what `gombit new` and `gombit make resource` emit (`scaffold/templates/`, `resourcegen/`, and the golden trees in `goldentest/testdata/golden/`), plus [ADR-016](../../../../docs/adr/016-model-first-resource-generation.md) for resource ownership. This started as the historical build plan §3.2. Use it when scaffolding or placing application code, not for the framework repo itself.

```
myapp/
├── cmd/server/main.go
├── cmd/gombit/main.go       # framework Cobra tree + product.RegisterCommands
├── internal/
│   ├── platform/            # app wiring the framework owns the shape of (AutoMigrate list)
│   ├── web/                 # embed.go for gombit build --embed
│   ├── commands/            # optional; gombit make command (M4-7)
│   ├── product/             # starter resource from gombit new (hand-written layout)
│   │   ├── product.go       # model
│   │   ├── handler.go       # Huma handlers (thin, over GORM)
│   │   ├── routes.go        # HTTP registration
│   │   └── commands.go      # RegisterCommands for the app CLI
│   └── task/                # a gombit make resource package (model-first, ADR-016)
│       ├── task.go          # model — user-owned, scaffolded once
│       ├── hooks.go         # customization hooks — user-owned, seeded once
│       ├── dto.gen.go       # DTOs + mappers — generator-owned, DO NOT EDIT
│       ├── handler.gen.go   # Huma handlers + Register — generator-owned, DO NOT EDIT
│       ├── .gombit-resource # marker gombit generate looks for
│       ├── service.go       # ONLY if --service
│       └── repo.go          # ONLY if --repo
├── database/migrations/     # versioned SQL + models.json registry
├── database/seeds/
├── config/
├── frontend/                # Vite React app
├── gombit.yaml
├── .env.example
├── go.mod
└── README.md
```

Rules:

- Routes are registered **explicitly** from `main.go` (`<pkg>.Register(app)`). No reflection discovery.
- Customize a model-first resource in its model, field policy, and `hooks.go` — never by editing `*.gen.go`; re-run `gombit generate` after changing the model.
- Management commands are registered **explicitly** from `cmd/gombit` via Cobra `AddCommand` / `cli.AddCommand`. No reflection discovery and no second command router.
- Route-registration edits (when generators exist) use `go/ast` and only append at a known registration point.
- Framework-owned endpoints (probes, metrics, OpenAPI) stay in the runtime; example-domain models must not leak into runtime packages.
- Split deploy is the default. Embed the frontend only via `gombit build --embed`.
- `.env.example` splits server secrets from `VITE_*` public values.
