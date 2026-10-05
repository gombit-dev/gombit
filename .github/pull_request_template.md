<!--
Fill in every section below. Delete this comment block before submitting.
See the agent working agreement in AGENTS.md (carried over from the historical
docs/GOMBIT_BUILD_PLAN.md §5)
for the definition of done this template encodes.
-->

## Summary

<!-- What does this PR add or change, and why? A sentence or a few bullets. -->

Closes #<!-- issue number -->

## Acceptance criteria

<!-- Copy the linked issue's AC checklist and mark what this PR satisfies.
     If any AC is intentionally not satisfied yet, say so and why. -->

- [ ]
- [ ]

## Scope notes

<!-- Anything explicitly deferred to a later issue/milestone. Delete this
     section if there's nothing to defer. -->

## Validation

<!-- Commands you ran locally, and the result. -->

- [ ] `go test ./...`
- [ ] `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run`
- [ ] Project `code-review` skill run against this diff

## Working agreement checklist

<!-- build plan §5 / AGENTS.md. Check what applies; if an item doesn't
     apply to this PR, leave it unchecked and say why instead of deleting it. -->

- [ ] New behavior has tests; DB-touching changes pass the SQLite + PostgreSQL + MySQL matrix
- [ ] Stable features ship docs and appear in an example app
- [ ] Extraction preserves contracts — refactor and move, don't rewrite code that already passes its tests
- [ ] Generators are idempotent/additive, AST-safe, and never overwrite user-owned files (if this PR touches generators)
- [ ] API changes regenerate the OpenAPI doc + TS client in this PR
- [ ] No secrets in generated frontend source; `VITE_*` is treated as public
- [ ] Scope stays inside this issue's milestone — no "battery" creep (events, scheduler, mail, gRPC, multi-tenancy, i18n)
- [ ] This PR links its issue and states which acceptance criteria it satisfies
