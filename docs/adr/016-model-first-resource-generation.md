# ADR-016: Model-First, Generator-Owned Resource Contracts

## Status

Proposed.

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

- **Single source of truth.** The GORM model plus explicit **field-policy
  metadata** is authoritative. Policy is *declared*, not inferred from code, and
  captures, per field: DB-backed?, in the request (writable)?, in the response
  (readable)?, create source (from request vs. set server-side), API-hidden.

- **Everything else is derived and generator-owned.** From the model + policy
  the generator produces the request/response DTOs, the model↔DTO mappers, the
  CRUD handler/plumbing, and the OpenAPI contract (with the TypeScript client
  downstream of OpenAPI as today). These land as regenerable `*.gen.go` files.

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

  A tenant/owner id is set inside `BeforeCreate`, not by mutating plumbing. A
  developer may replace the whole generated CRUD implementation, but that is an
  explicit opt-out of the generated guarantees, not a silent edit the framework
  still reasons about.

- **Drift becomes staleness, checked by regeneration — not inspection.**
  `gombit generate --check` regenerates to a scratch filesystem and byte-compares
  against the committed `*.gen.go`; a per-PR CI gate fails on any difference.
  Because the artifacts are derived, they cannot semantically disagree with the
  model; the only failure is "a generated file was edited or not regenerated,"
  which a byte comparison catches with no AST reasoning.

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
- Generated apps in the wild are not a concern (pre-v0.1; `gombit new` writes
  them on demand), so the migration cost is low.
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
