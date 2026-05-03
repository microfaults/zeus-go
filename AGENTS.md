## Project Intention

Zeus-go is the **execution plane** for faults-lab. Manteion (control plane) issues commands; zeus runs them and reports stats. Two surfaces:

- **k6 workflow runner** — drives broad, session-shaped traffic against target services using a JSON-defined DSL v2 tree-walking engine. One k6 sidecar per workflow.
- **Precision attack launcher** — uses [Vegeta](https://github.com/tsenart/vegeta) for narrow, rate-controlled hits against specific endpoints outside the workflow path (e.g., spike a single API while the main workflow keeps running).

Zeus does not decide *what* to run or *when*. It accepts run requests, validates them against datasets, executes, and exposes stats. Phase ordering, rule toggling, and analysis live in manteion-go.

## Ecosystem

- **atropos-go** — Per-service Go SDK. Embeds in target services. Owns OTel instrumentation, fault injection (inline/network/resource), and cache-box modal middleware. Reads the `atropos.workflow` baggage label that zeus injects.
- **manteion-go** — Central control plane. Pushes evaluator rules and cache-box mode changes to atropos SDK instances. Orchestrates experiments. Calls zeus's HTTP API to drive workflows. Owns policy evaluation (migrated from zeus's old archer policy engine).
- **service-beds** — Go HTTP recreations of Google Online Boutique (13 services) with atropos embedded. The default load target.
- **zeus-go** (this repo) — Workflow runner, dataset store, precision attack launcher, stats. Stateless except for in-memory run/dataset/workflow registries.

## Research Context

UCSC Faults Lab (Peter Alvaro's group) — interventional performance attribution for microservices. The experiment loop:

1. Manteion uploads dataset + workflow to zeus.
2. Manteion calls `POST /api/v1/workflows/{id}/runs` for the **baseline** phase. Zeus drives k6 at constant arrival rate; manteion polls `GET /runs/{id}/stats`.
3. Manteion mutates atropos rules (e.g., freeze productcatalogservice at cache-box replay mode), then calls zeus again for the **isolation** phase with the same dataset + workflow + VU count + duration.
4. Manteion computes `Δ_service = baseline_latency - isolated_latency`, repeats for combination phases, detects superadditive contention via `Δ_combined ≠ Σ Δ_individual`.

Zeus's invariant: same workflow + dataset + duration + VU count → same load profile, regardless of the atropos rule state. The **only** variable across phases is what atropos is doing inside the target services. This is why zeus must run open-loop (constant arrival rate, not closed-loop VU iterations) — see "Known confounds" in `../VISION.md`.

Target venues: ACM SoCC, USENIX ATC, ICPE.

## Architecture

- **Go service** — REST API on stdlib `net/http` with Go 1.22+ method-path routing (`mux.HandleFunc("METHOD /path", h)`). Entry: `cmd/zeus/main.go`. Single binary.
- **k6 sidecars** — Generic `k6/runner.js` driven by JSON workflow files. Engine in `k6/scripts/lib/engine.js`; templating in `k6/scripts/lib/template.js`. Sidecars run in their own containers (see `docker-compose.yml`, `kubernetes-manifests/`).
- **k6 executor** — `constant-arrival-rate` (open-loop). Holds offered RPS constant across phases so cache-box freezes don't inflate throughput. A run with `dropped_iterations > 0` is invalid for delta math — surfaced via `zeus_run_dropped_iterations_total`.
- **Vegeta** — Used in-process (Go library), not as a subprocess. See `internal/attacker/`. Targets are constructed per attack request with optional dedup-bypass header/query mutation.
- **OpenAPI 3.1** — Generated from handler annotations via `swag` v2. Spec at `docs/swagger.{yaml,json}`, regenerated via `make openapi`. CI gates on `make openapi-check`. See `docs/openapi-conventions.md`.
- **Storage** — In-memory only. Workflows, runs, datasets, and attack registries are all `sync.RWMutex`-guarded maps. Datasets use `patrickmn/go-cache` for TTL eviction. No PostgreSQL — zeus is intentionally stateless across restarts; manteion is the source of truth.

## Key Domains

- **Workflows** (`internal/workflow/`) — DSL v2 documents (JSON tree). Stored in-memory keyed by name+version. Schema validation runs before any run starts.
- **Runs** (`internal/run/`) — Lifecycle state machine: `starting → validating → running → completing → completed | stopped | failed | rejected`. `rejected` is dataset-handshake-specific (manteion may retry with a different dataset; should NOT retry on `failed`). Filtered store supports listing by `experiment_id`, `workflow_id`, `status`, `phase` (from `labels.phase`). Each run carries a free-form `labels` map (e.g., `{"phase": "isolation-1a", "frozen_service": "productcatalog"}`) that flows into Prometheus snapshots and SSE events for downstream join keys.
- **Datasets** (`internal/dataset/`) — Named pool collections (`{users: [...], products: [...]}`). NDJSON streaming upload at `POST /api/v1/datasets/{id}/upload`. TTL-based eviction. Validated against a workflow's `data_schema.pools` block at run-start.
- **Attacks** (`internal/attacker/`) — Vegeta-orchestrated rate attacks. Each attack carries `experiment_id`, `run_ref` (optional FK to a concurrent workflow run), and `workflow_label` (defaults to the linked run's label). These let manteion join attack metrics with run metrics under the same experiment phase. List filters: `?status=running&experiment_id=X&run_ref=Y`. Per-attack stats endpoint.
- **Stats** (`internal/stats/`) — Prometheus custom registry under `zeus_*` namespace. Per-run snapshots with cardinality pruning (run_id label removed on run end). Exposed at `/api/v1/metrics`.
- **SSE** (`internal/sse/`) — Run event broker. Event types: `step.ok`, `step.drop` (with `reason: expectation` etc.), `iteration.done`, `stats.snapshot` (periodic). Resumable via `Last-Event-ID`. Endpoint: `/runs/{id}/events`. Manteion uses this for live dashboards, not aggregated stats collection — for the latter, poll `/runs/{id}/stats`.
- **Trace** (`internal/trace/`) — W3C Baggage helpers. Injects `meta-trace-id` and `atropos.workflow` on every outbound request from both k6 and Vegeta. The workflow label sources from `workflow_label` field on `POST /runs` (defaults to the workflow's `name`) → `ZEUS_WORKFLOW_LABEL` env on the k6 container → `runner.js` init → engine → per-request baggage header. Same path for the Vegeta targeter via `InjectLabeledBaggage(header, {"meta-trace-id": id, "atropos.workflow": label})`. This wiring resolves atropos ambiguity A9 — without it, atropos's per-workflow rule scoping (`{atropos.workflow: browse}` label match) is inoperable. See `docs/api-contract.md` § "atropos.workflow baggage wiring."
- **Dedup** (`internal/dedup/`) — Idempotency-bypass strategies for precision attacks: `X-Idempotency-Key` header injection or query-param nonce.
- **ID** (`internal/id/`) — Shared cryptographic hex ID generation. Use this, not ad-hoc UUIDs.

## DSL v2 (workflow grammar)

JSON tree of typed nodes. Seven node types:

| Type | Purpose |
|---|---|
| `sequence` | Ordered children. Later children read earlier extracts via `{{steps.<id>.*}}`. |
| `parallel` | Concurrent children (k6 `http.batch` for request-only fan-outs; sequential fallback otherwise). `wait`: `all` / `any` / `{n_of_m: K}`. |
| `delay` | Explicit sleep. `{min_ms, max_ms}` or `{persona_key}`. |
| `optional` | Probability gate (random per iteration). Probability is numeric or persona-keyed string. |
| `request` | Leaf. HTTP call with templating, variants, expect/extract, before/after delays. |
| `repeat` | Bounded loops. `count` / `count_template` / `while+max`. Iteration K+1 sees K's extracts via shared loop scope. |
| `if` | Deterministic conditional. `condition` (template-truthy) → `then` / optional `else`. Scope-transparent. |

Variant sampling: a `request` may carry weighted `variants`, each with a sparse dot-path patch (`body.region`, `headers.X-Foo`, `path`, `data.<pool>.<field>_override`). Engine picks one per iteration.

Extract scope: tree-shaped. Sequence children see siblings' extracts; parallel children get independent scopes that merge back under the parallel's id; optional/if/repeat are scope-transparent or merge under their own id.

Full spec: `docs/workflow-dsl-v2.md`.

## HTTP API (summary)

All routes under `/api/v1`. JSON request/response. Errors: `api.ErrorResponse{error: "..."}`. 201 on create, 404 via `errors.Is(err, ErrNotFound)`, `[]` (never `null`) on empty lists.

| Group | Endpoints |
|---|---|
| Workflows | `POST /workflows`, `GET /workflows`, `GET/DELETE /workflows/{id}`, `POST /workflows/{id}/validate?dataset=<id>` |
| Runs | `POST /workflows/{id}/runs`, `GET /runs`, `GET/DELETE /runs/{id}`, `GET /runs/{id}/stats`, `GET /runs/{id}/events` (SSE) |
| Datasets | `POST /datasets`, `GET /datasets`, `GET/DELETE /datasets/{id}`, `POST /datasets/{id}/upload` (NDJSON streaming), `GET /datasets/{id}/sample` |
| Attacks | `POST /attacks`, `GET /attacks`, `GET/DELETE /attacks/{id}`, `GET /attacks/{id}/stats` |
| Stats | `GET /metrics` (Prometheus exposition), `GET /metrics/summary` |
| Health | `GET /healthz`, `GET /readyz`, `GET /status` (dual-mounted at root and under `/api/v1`) |

Two-phase data delivery is the default: upload a dataset, then reference it by id in the run create. Single-phase (`dataset_inline`) capped at 1 MiB for ergonomic local testing.

Full per-route detail: `docs/api-contract.md` (narrative + state machine) and `docs/swagger.yaml` (generated).

## Manteion ↔ Zeus integration

Manteion is the canonical caller. The integration is one-directional (manteion → zeus over HTTP); zeus never calls back into manteion. A typical experiment loop, from zeus's perspective:

1. **Workflow registration.** `POST /workflows` with the DSL v2 document. Zeus assigns `id` and returns `{id, name, version, created_at}`. Subsequent runs reference the workflow by id.
2. **Dataset upload.** `POST /datasets` to register, then `POST /datasets/{id}/upload` with `Content-Type: application/x-ndjson` to stream rows. Each NDJSON line is `{"pool": "<name>", "rows": [...]}`. Manteion projects these rows from cache-box exports (real request bodies grouped by trace) — see "three-signal data strategy" in `../VISION.md`.
3. **Run creation.** `POST /workflows/{id}/runs` with:
   ```json
   {
     "experiment_id":  "<manteion-assigned>",
     "dataset_id":     "<from step 2>",
     "vus":            50,
     "duration_s":     300,
     "persona":        "balanced",
     "labels":         { "phase": "isolation-1a", "frozen_service": "productcatalog" },
     "workflow_label": "browse"
   }
   ```
   Returns `202 {run_id, status: "starting", k6_job_name}`. `422` with `state: rejected` means data validation failed (manteion may retry with a different dataset). `400`/`409` are unrecoverable.
4. **Live tail.** Optional. `GET /runs/{id}/events` SSE stream for dashboards and progress.
5. **Stats poll.** `GET /runs/{id}/stats` returns the structured per-run snapshot (latency histograms, drop counts, variant pick distribution, dropped-iterations count). Manteion polls until the run reaches a terminal state.
6. **(Optional) Concurrent precision attack.** `POST /attacks` with `run_ref: <run_id>` and `experiment_id` matching the workflow run. Inherits `workflow_label` from the run. Used for "spike one endpoint while the broad workflow runs."
7. **Cancellation.** `DELETE /runs/{id}` transitions to `stopped` (terminal). Pending phase transitions on manteion's side abort accordingly.

**Validity gate for delta math.** A run is **invalid** for `Δ_service = baseline − isolated` math if `dropped_iterations > 0` — k6's open-loop arrival rate failed to keep up. Zeus surfaces this via `zeus_run_dropped_iterations_total{run_id}`. Manteion gates the analysis on this metric and re-runs the phase if it trips.

**State drift mitigation (out of zeus scope, called by manteion between phases).** For stateful target services (e.g., service-beds cartservice with Redis, checkoutservice with Kafka), manteion runs `service-beds/scripts/reset-online-boutique-state.sh` between phase runs to keep the load profile reproducible. Zeus has no hook for this — the contract is "manteion ensures the target is in a known state before calling `POST /runs` for the next phase."

## Targeting a system (service-beds, DSB, anything else)

What zeus needs from a target system to drive load against it:

- **Reachable HTTP endpoints** at the workflow's `base_url` (overridable via `BASE_URL` env on the k6 container). All workflow `request` nodes are HTTP — gRPC services are out of scope unless fronted by an HTTP gateway (DSB Social Network does this with `nginx-thrift`).
- **Atropos SDK embedded** in services that will be frozen. Required for cache-box experiments to be meaningful — without atropos, the `atropos.workflow` baggage zeus injects has no listener and per-workflow rule scoping is inoperable. Optional only if running zeus purely for traffic generation (no isolation experiments).
- **Prometheus exposition** on services manteion will ask about for load sizing (`rate(http_requests_total[1h])`). Optional but required if you want manteion to compute `vus` + `estimated_rps_per_vu` from production rates rather than hand-tuning.
- **OTel trace export** to a collector manteion can read. Optional but required if you want manteion to project workflow tree shapes from real traffic rather than hand-authoring DSL v2 trees.
- **A reset/init mechanism** (operator-supplied script or admin endpoint). Called by manteion between phase runs; zeus does not invoke it. Examples: `service-beds/scripts/reset-online-boutique-state.sh` for Online Boutique; `socialNetwork/init_social_graph.py` for DSB.
- **A workflow document and dataset**. Either hand-authored against the DSL v2 spec or projected by manteion from cache-box + traces. The dataset's pool names must satisfy the workflow's `data_schema.pools` block — zeus rejects the run on schema mismatch.

Two concrete examples in this repo:

- **Online Boutique** (`k6/flows/online-boutique/`): `browse.json` and `checkout.json` workflows. Dataset: `cmd/gen-dataset` synthesizes 50 users + 9 hardcoded products + currencies. Targets the `service-beds` Go reimplementation.
- **DSB Social Network** (`docs/examples/deathstarbench-social-network.md`): worked example showing parallel follow fan-out, weighted body variants, and cross-step extracts against `nginx-thrift`. Dataset would be projected from cache-box dumps of `/wrk2-api/*` traffic — see the doc's "Where manteion sources this data" section.

## Observability

- **Metrics:** `zeus_*` Prometheus namespace, custom registry. Per-run cardinality controlled by adding `run_id` label only while a run is active; pruned on terminal state. Never label by `meta_trace_id` (per-run random 32-hex would destroy Prometheus). `attack_id` is permitted because attacks are short-lived and bounded by the active set. Catalog in `docs/api-contract.md` under "Prometheus metrics catalog."
- **Run events:** SSE per-run stream. Manteion subscribes to tail a run's progress; aggregated stats come from `/runs/{id}/stats`, not the SSE stream. Resumable via `Last-Event-ID`.
- **Tracing baggage:** Every outbound request from k6 and Vegeta carries `meta-trace-id` (UUID per VU iteration / per attack request) and `atropos.workflow` (workflow label). Atropos SDKs read these to scope rules and correlate trace spans with experiment phases. See `internal/trace/context.go` for the helper.
- **Run validity contract:** A run with `dropped_iterations > 0` (offered RPS unmet under open-loop arrival) is not valid for delta math. Manteion gates on `zeus_run_dropped_iterations_total` before computing `Δ_service`.

## Code Style

- Go 1.25. External deps minimized. Avoid adding new ones without a reason — `vegeta` and `go-cache` are the meaningful runtime imports.
- Follow the `Server` struct pattern: `Handler()`, `routes()`, JSON helpers (`writeJSON`, `writeError`, `readJSON`) in `internal/api/server.go`. Pass dependencies via `api.Deps{}` constructor.
- String IDs throughout. `internal/id` for new ID generation — do not roll UUIDs ad hoc.
- `Validate() error` methods on domain types.
- Table-driven tests; keep them in the same package as the code under test.
- `json.RawMessage` for opaque/extensible fields (workflow tree blobs, attack target specs).
- For HTTP handlers, every endpoint must carry full swag annotations (see `docs/openapi-conventions.md`) — `@Summary`, `@Description`, `@Tags`, `@Produce`, `@Accept` (if body), `@Success`, `@Failure`, `@Router`. CI fails on stale `docs/swagger.yaml`.

## PR Instructions

- Title format: `[zeus-go] <Title>` (matches the manteion-go convention; verify against repo CI rules if unsure).
- Before committing: `go fmt ./... && go vet ./... && go build ./... && go test ./...`.
- If you touched a handler, regenerate the OpenAPI spec and commit `docs/swagger.{yaml,json}` together with the code change: `make openapi`. CI runs `make openapi-check` and fails on diff.
- New k6 engine features should ship with a fixture under `k6/flows/_example/` and a smoke run against `httpbin.org` (or equivalent) to verify the engine walks correctly.
- Branches under `claude/<adjective-noun>` are agent-spawned and live in `.claude/worktrees/<name>/` — they are gitignored and safe to remove with `git worktree remove` once merged.

## Reference Files

Implementation:
- `cmd/zeus/main.go` — Entry point, dependency wiring, graceful shutdown
- `cmd/gen-dataset/main.go` — Synthetic dataset generator (online-boutique products + Luhn-valid fake users) — NDJSON for upload, JSON for k6 file mode
- `internal/api/server.go` — Server struct, routes, JSON helpers, dependency injection
- `internal/run/registry.go` — Run lifecycle state machine and filtered store
- `internal/workflow/store.go` — DSL v2 schema validation
- `internal/dataset/store.go` — go-cache TTL store, NDJSON ingest
- `internal/stats/registry.go` — Prometheus custom registry, cardinality pruning
- `internal/attacker/attacker.go` — Vegeta orchestration, baggage injection point
- `internal/trace/context.go` — W3C Baggage helpers (`InjectLabeledBaggage`)
- `k6/runner.js` — Generic k6 entry point, env-driven config
- `k6/scripts/lib/engine.js` — DSL v2 tree-walking engine
- `k6/scripts/lib/template.js` — Template expression evaluator + truthiness helpers
- `Makefile` — `make openapi`, `make openapi-check`, `make test`, `make build`

Docs:
- `docs/workflow-dsl-v2.md` — Full DSL v2 spec, node types, scope rules, prior-art positioning
- `docs/api-contract.md` — Narrative HTTP API with state machine
- `docs/openapi-conventions.md` — Required swag annotations per handler
- `docs/swagger.{yaml,json}` — Generated OpenAPI 3.1 spec (do not hand-edit)
- `docs/examples/deathstarbench-social-network.md` — Multi-service workflow showing parallel + variants
- `docs/cleanup-legacy-k6.md` — Legacy file disposition spec

Companion repos:
- `../VISION.md` — Top-level faults-lab story; component composition; experiment vs workflow; three-signal data strategy; known confounds
- `../atropos-go/AGENTS.md` and `../atropos-go/VISION.md` — SDK boundary, cache-box, fault taxonomy
- `../manteion-go/AGENTS.md` — Control plane, rule store, experiment orchestration
- `../service-beds/AGENTS.md` — Default target application set
