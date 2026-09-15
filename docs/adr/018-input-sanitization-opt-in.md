# ADR-018: Request-input HTML sanitization is opt-in

## Status

**Accepted.** Implements the decision leg of issue #271 (PERF-13), the fifth
child of the framework-tax allocation milestone #264 (PERF-6).

## Context

`xssMiddleware` ran on every request in the default `framework.App` pipeline. On
JSON `POST`/`PUT`/`PATCH` it buffered the whole body, decoded it, stripped HTML
tags from every string value, and re-encoded it before Huma read the same
bytes; on `GET` it stripped query values.

Two problems.

**It costs allocations on the hot path.** The cumulative ablation attributes
**+5 allocs/op** to the layer on a valid or invalid POST (`io.LimitReader`,
`io.ReadAll`, `bytes.NewReader`, `io.NopCloser`, and `mime.ParseMediaType`'s
param map), and Huma already decodes the body twice (once into `any` for schema
validation, once into the typed struct) — the sanitizer adds a third full read
on the fast path and a fourth decode on the slow path. Removing it from the
stack after the #241 content-type fast path took a valid POST from 46 to 42
allocs/op.

**It rewrites API input, which is the wrong layer for XSS defense.** XSS is an
*output-encoding* concern, and the controls Gombit actually ships are all on
output: the generated React frontend escapes text by default (JSX), the
framework admin is a React SPA that renders values as React text — not HTML, and
there is no server-rendered `html/template` admin path — a JSON API response is
not an HTML sink, and the response `Content-Security-Policy` is a backstop. The
actual defense is at render time, in the output context that matters. Stripping
markup on ingress instead:

- silently corrupts data an app must store faithfully — a comment containing
  `x < y`, a stored `<b>bold</b>`, a code snippet — the class of bug behind
  #118, #201, and #232;
- gives false confidence (it does not encode for any specific output context);
- forces webhook paths to opt out via `WithRawBodyPaths` because re-encoding the
  body breaks signature verification.

## Decision

**Move request-input sanitization out of the default pipeline; offer it as
explicit, narrower opt-ins.**

1. **Default pipeline has no `xss` layer.** A default `framework.App` does not
   read, decode, or rewrite request input for sanitization. Huma decodes the
   body as it always did. Nothing changes on the response/output side.
2. **The request-size bound is separated, not removed.** The old sanitizer
   incidentally capped JSON bodies at 8 MiB (it buffered the body to strip it).
   That bound is a memory-safety concern, not a sanitization one, so it now
   lives in its own always-on `request_body_limit` middleware: an oversized JSON
   `POST`/`PUT`/`PATCH` is rejected with a D10 413 before any handler runs
   (including a raw `app.Router()` handler that calls `ShouldBindJSON`), and a
   streamed body is bounded by `http.MaxBytesReader`. It never decodes or
   mutates the body, so an app gets the bound without opting into input
   rewriting. `WithRawBodyPaths` stay exempt, exactly as under the old sanitizer.
3. **Per-app opt-in:** `Security.SanitizeInput`
   (`GOMBIT_SECURITY_SANITIZE_INPUT`, default `false`) installs today's
   middleware unchanged, including its `password`-key exemption, the #201
   unclosed-element handling, and `WithRawBodyPaths` exemption semantics.
4. **Per-field opt-in:** `framework.SanitizeHTML(s string) string` exports the
   existing `stripHTML` behavior so a handler can strip one untrusted field
   without re-enabling the whole pipeline.
5. **Document the posture change** in `docs/security.md` (§ Input sanitization),
   `docs/router.md`, `docs/lifecycle.md`, the build plan M1-8 entry, and the
   `examples/router` example: what the default protects against on output
   (unchanged) and when to enable ingress stripping.

## Consequences

- **Breaking for apps that relied on ingress stripping.** An app that assumed
  request input was HTML-stripped now receives it verbatim and must ensure it
  escapes on output (the React frontend and admin already do). Apps that want
  the old behavior set `GOMBIT_SECURITY_SANITIZE_INPUT=true`. Recorded in the
  CHANGELOG as a breaking change.
- **Input values are preserved by default.** The default middleware no longer
  mutates JSON string or query values, so `{"description":"x < y"}` and
  `{"note":"<b>bold</b>"}` reach handlers with their values intact — the
  #118/#201/#232 class of input-mangling bugs cannot occur on the default path
  because nothing mangles input there. This is value fidelity, not byte-for-byte
  fidelity: the response envelope re-encodes JSON. A handler that needs the
  exact original bytes (a webhook verifying a signature) reads the raw body via
  `WithRawBodyPaths`.
- **Allocation win.** The default POST path drops ~4–5 allocs/op; combined with
  the milestone's other children it reaches the #264 "sanitizer opt-in" target
  column.
- **#201 stays fixed on the opt-in path.** The unclosed-dangerous-element
  data-loss fix (`stripHTMLUnclosed`) is retained unchanged and is exercised
  whenever `Security.SanitizeInput` is on; it is no longer on the default path
  because there is no default sanitization to lose data.

## Alternatives considered

- **Keep it on by default:** rejected. It taxes every write request for a
  defense applied at the wrong layer, and it corrupts faithful input.
- **A resourcegen field-level `sanitize` tag / model option** so specific
  generated fields strip on write: deferred as a follow-up. `SanitizeHTML`
  covers the handler-level need today without new generator surface.
- **Non-JSON (form/multipart) coverage:** out of scope, as it was before — the
  public API path is JSON/Huma.
