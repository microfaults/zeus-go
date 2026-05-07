# OpenAPI Annotation Conventions

All Go handlers in this repo use [swaggo/swag](https://github.com/swaggo/swag) v2 to generate OpenAPI 3.1. Zeus is the execution plane for faults-lab; the generated spec describes the contract manteion-go (and any other client) integrates against. The narrative + state-machine view of the API lives in `docs/api-contract.md`; per-route detail lives in `docs/swagger.yaml`.

## Toolchain

- `swag` v2.0.0-rc5 (pinned; v2.0.0 has not been released yet). Install with `go install github.com/swaggo/swag/v2/cmd/swag@v2.0.0-rc5`. Note: `swag --version` self-reports `v2.0.0` regardless of which v2 RC is installed; trust the `go install` command pin (`@v2.0.0-rc5`), not the binary's banner.
- The general-info annotations use swag v2's `@servers.url` / `@servers.description` directives (paired by ordinal position) rather than the deprecated `@host` / `@BasePath` / `@schemes` triplet. Re-evaluate the syntax once swag publishes a stable v2.0.0.
- The post-processor at `scripts/strip-empty-externaldocs.py` strips empty `externalDocs:` blocks emitted by swag v2.0.x; remove this once swag publishes a release that omits empty stubs.
- Specs are committed at `docs/swagger.{yaml,json}` and validated in CI on pushes to `main`/`develop`.

## Required annotations on every handler

- `@Summary` — one-line imperative ("Create run", "List workflows")
- `@Description` — one paragraph; clients render this as JSDoc
- `@Tags` — one of: `runs`, `workflows`, `datasets`, `attacks`, `metrics`, `health`
- `@Produce json` (or `text/event-stream` for SSE)
- `@Accept json` (for routes that take a body)
- `@Success` and `@Failure` for every documented status code
- `@Router` with method in brackets

## Body and response types

- Use Go types directly: `{object} workflow.Workflow`, `{array} run.Run` — `swag init`'s `--dir` flag is configured to walk `internal/{api,run,dataset,workflow,stats}` so subpackage types resolve.
- Use `api.ErrorResponse` for all 4xx/5xx responses.
- For path params (zeus uses `{id}` and `{run_id}` patterns), declare with `@Param id path string true "Workflow ID"` (or similar — name the entity).

## Streaming endpoints (SSE)

`/runs/{run_id}/events` is the SSE endpoint:

- `@Produce text/event-stream`
- Event types are listed in the `@Description` text (`step.ok`, `step.drop`, `iteration.done`, `phase.transition`).
- `Last-Event-ID` header is supported for resumable streams; document via `@Param Last-Event-ID header string false "..."`.

## Health endpoints

`/healthz` and `/readyz` are dual-mounted: at root (for direct k8s probes) and under `/api/v1/...` (so OpenAPI clients composing `servers[0].url + path_key` reach a real route). The `@Router` path stays bare (`/healthz`) — same convention as every other endpoint. The dual mount happens in `internal/api/server.go`'s `routes()`.

## Regenerating the spec

Run `make openapi` from the repo root.

Commit `docs/swagger.{yaml,json}` together with handler edits. CI fails if the spec is stale.
