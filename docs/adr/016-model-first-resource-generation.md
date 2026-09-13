# ADR-016: Model-First, Generator-Owned Resource Contracts

## Status

Accepted.

## Context

A generated resource today carries several **independently hand-editable
representations of the same contract**: the GORM model, the request DTO, the
create mapper, the response DTO, the response mapper, and (via Huma) the OpenAPI
schema. `gombit make resource` emits a coordinated set, then hands the
handler/DTOs to the developer as human-owned files — while the framework keeps
assuming invariants about them. When the model later changes, nothing keeps the
representations in agreement, producing the failure modes in
[#218](https://github.com/gombit-dev/gombit/issues/218): a new field is rejected
on input (`422 unexpected property`), a field silently disappears from
responses, a required NOT NULL column is silently zero-filled on create, and a
field present in every DTO but unmapped is accepted and discarded.

[PR #298](https://github.com/gombit-dev/gombit/pull/298) tried to close that gap
by proving, after the fact, that the hand-owned representations still agree —
by parsing the generated Go. Adversarial review walked it up an escalating
ladder, each fix exposing the next semantic layer:

```
DTO membership → composite-literal presence → named constructor → constructor+Create occurrence
→ ordered syntax → alias checks → exact grammar → Huma-schema reflection → mapper-arrow analysis …
```

That ladder is the architectural signal, not reviewer pedantry: once a check
needs JSON tags, GORM column names, constructor/response-mapper RHS expressions,
receiver identity, call ordering, aliases, pointer receivers, `defer`, branches,
server-managed mutations, relationships, and custom persistence calls, it has
stopped being a drift guard and become an ad-hoc static analyzer for arbitrary
Go. The root cause: **the generator relinquished ownership of CRUD plumbing and
then tried to recover its invariants by inspection.** #298 was closed as a dead
end, and its `resourcecheck` approach is not to be revived or extended.

## Decision

Resource contracts become **model-derived and generator-owned. Human
customization happens through explicit extension points (hooks/services), never
by editing generated CRUD plumbing.** This is not "write a better drift
checker" — it removes the need for one by removing the multiple hand-editable
representations it was reconciling.

This ADR locks the **semantics** below. The exact surface syntax (field-policy
tags vs. a resource-definition DSL, hook signatures, file names) is an
implementation detail to be settled while building the epic
([#352](https://github.com/gombit-dev/gombit/issues/352)); it must not be
fossilized here.

- **Single source of truth, one owner per fact.** The GORM model plus explicit
  **API field-policy metadata** is authoritative, and policy must not duplicate
  facts the schema already owns:

  ```
  GORM schema owns:   DB-backed / relationship / primary key / column name / nullability / default
  Gombit policy owns: writable?  readable?  create source = request | server  API-hidden?
  ```

  Policy is *declared*, not inferred from code. Persistence facts are read from
  the GORM schema, never re-stated in policy (re-stating them would recreate the
  original multiple-representations problem).

- **Everything else is derived and generator-owned.** From the model + policy
  the generator produces the request/response DTOs, the model↔DTO mappers, and
  the CRUD handler/plumbing, landing as regenerable `*.gen.go` files. OpenAPI is
  **not** a separate representation Gombit maintains: the generated Huma DTOs and
  handlers are the inputs Huma emits OpenAPI from, with the TypeScript client
  downstream of that, exactly as today:

  ```
  GORM model + policy → generated Huma DTOs + handlers → Huma → OpenAPI → TS client
  ```

- **Ownership boundary.** `*.gen.go` is generator-owned and is **not a
  customization surface**. Human code is the model (with policy) and a
  human-owned hooks/services file. Editing generated plumbing is unsupported.

- **Customization via explicit extension points.** Business logic and
  server-derived values enter through declared hooks (`BeforeCreate` /
  `AfterCreate` / … , or service interfaces where warranted). The generated
  handler owns the invariant sequence and calls the hooks, e.g.:

  ```go
  row := bookFromCreateInput(input)
  if err := hooks.BeforeCreate(ctx, &row, input); err != nil { return err }
  if err := db.WithContext(ctx).Create(&row).Error; err != nil { return err }
  ```

  A tenant/owner id is set inside `BeforeCreate`, not by mutating plumbing.
  (A mode that lets a resource opt out of generated CRUD entirely — the
  generator emits no plumbing for it and the developer owns the implementation,
  forgoing the generated guarantees — is possible future work, out of scope for
  this ADR.)

- **Drift becomes staleness, checked by regeneration — not inspection.**
  `gombit generate --check` regenerates to a scratch filesystem and byte-compares
  against the committed `*.gen.go`; a per-PR CI gate fails on any difference.
  Because the artifacts are derived, they cannot *independently* drift through
  human edits: `generate --check` detects stale or modified generated output with
  no AST reasoning. It proves freshness/determinism, **not** semantic correctness
  — a generator bug can still emit wrong DTOs or mappings, so generator
  correctness stays enforced by the generator's own test suite.

Two boundaries are non-negotiable acceptance criteria for the implementation:

1. Changing a model field and its policy requires **no manual synchronization**
   across DTOs, mappers, handlers, or OpenAPI — regeneration is the
   synchronization mechanism.
2. **Editing generated plumbing is not a supported customization path.**

This does **not** re-litigate [ADR-011](011-contract-layer-huma.md): Huma-typed
handlers remain the emitted API contract and OpenAPI is still emitted from them.
The change is that those Huma types are *generated from the model+policy* rather
than hand-owned and later inspected.

## Consequences

- `gombit make resource` changes shape: it emits generator-owned `*.gen.go`
  (DTOs, mappers, CRUD handler) plus a human-owned hooks file, instead of a
  single human-owned handler. The "generate once, then it's entirely yours"
  model for CRUD plumbing is retired.
- The `resourcecheck` package and the generated `*_drift_test.go` are removed as
  superseded; the drift concern is handled by `gombit generate --check`.
- Server-managed columns move from an opt-out list + post-construct assignment to
  an explicit `BeforeCreate`-style hook; field policy replaces the
  `<snake>_contract.go` opt-out lists.
- A new per-PR CI gate (`gombit generate --check`) is required; generated files
  are committed and must stay fresh.
- **This is a breaking change** for the generated-resource source layout and
  customization model, and it must be documented as one — not treated as
  non-breaking because users "can regenerate." HTTP contracts are preserved where
  possible, but generated CRUD handlers are no longer human-owned customization
  points: customization moves to explicit hooks/services. Regeneration can
  *overwrite* the old boundary, so anyone who customized a generated handler
  needs an **actual migration path** (move edits into hooks/services), not a
  "re-run the generator" hand-wave. Only the in-repo cost is small, because
  `gombit new`/`make resource` write apps on demand rather than committing them
  here; that says nothing about downstream users who already customized handlers.
- This is a `post-v0.1` epic (#352), larger than one PR: this ADR + field-policy
  design + generator rework + `generate --check` + hooks + removal of the
  human-owned-handler path. Agents should not resurrect an inspection-based drift
  guard as an alternative.

## References

- Issue [#352](https://github.com/gombit-dev/gombit/issues/352) — `[RESGEN-1]`
  Model-derived, generator-owned resource contracts (the epic this ADR governs)
- Issue [#218](https://github.com/gombit-dev/gombit/issues/218) — the original
  user-visible failure modes (stays open; broad contract-drift superseded here)
- PR [#298](https://github.com/gombit-dev/gombit/pull/298) — the inspection-based
  drift-guard experiment, closed as an architectural dead end
- [ADR-011](011-contract-layer-huma.md) — Huma-typed handlers are the contract
  source of truth (unchanged; the DTOs are now model-derived)
- Design doc §27 (field grammar) and §3.2 (feature-package layout),
  `docs/GO_FULLSTACK_FRAMEWORK_DESIGN.md`
