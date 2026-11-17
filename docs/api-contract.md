# Zeus Control-Plane API Contract

Zeus is the execution plane for faults-lab. Manteion issues commands to it. This document is the integration contract between the two.

All paths are under `/api/v1`. Request and response bodies are JSON unless noted. Timestamps are RFC 3339. Status codes follow REST conventions.

## Run lifecycle state machine

A run has one of eight states:

```
POST /runs
    → starting     (k6 container created, dataset binding queued)
    → validating    (data schema validation running)
    → running       (k6 executing iterations; interim stats on /stats and the event stream)
    → completing    (k6 exited; zeus reads its summary export)
    → completed     (terminal; final stats from k6's summary export)
       | stopped    (terminal, user cancelled via DELETE; stats gathered up to the stop are kept)
       | failed     (terminal, runtime error after execution started)
       | rejected   (terminal, data validation failed; no k6 ever ran)
```

`rejected` is dataset-handshake-specific. Manteion may retry with a different dataset on `rejected`; it should not retry on `failed`.

## Endpoint catalog

### 1. Workflow control

#### Register a workflow

```
POST /api/v1/workflows
```

```json
// request
{
  "workflow": { /* DSL v2 document, see workflow-dsl-v2.md */ },
  "overwrite": false
}

// response 201
{
  "id": "<workflow_id>",
  "name": "dsb-socialnet-compose-session",
  "version": 2,
  "created_at": "2026-04-11T12:00:00Z"
}

// 409 if name exists and overwrite != true
// 400 if schema validation fails, with details: [...]
```

#### List workflows

```
GET /api/v1/workflows
```

```json
// response 200
{
  "workflows": [
    { "id": "...", "name": "...", "version": 2, "targets": [...], "updated_at": "..." }
  ]
}
```

#### Fetch a workflow

```
GET /api/v1/workflows/{id}
```

```json
// response 200
{ "id": "...", "workflow": { /* DSL v2 doc */ }, "created_at": "...", "updated_at": "..." }
// 404 if not found
```

#### Remove a workflow

```
DELETE /api/v1/workflows/{id}
```

```
204 — deleted. Fails 409 if active runs reference it.
```

#### Validate workflow against a dataset

```
POST /api/v1/workflows/{id}/validate
```

```json
// request — either dataset_id or inline
{ "dataset_id": "..." }
// OR
{ "dataset": { "users": [...], "products": [...] } }

// response 200
{ "ok": true, "warnings": [] }

// response 422
{ "ok": false, "missing": ["pools.products.id"], "errors": ["users pool has 3 rows, min_size is 100"] }
```

#### Start a run

```
POST /api/v1/workflows/{id}/runs
```

```json
// request
{
  "run_id":          "<optional, server-assigned if empty>",
  "experiment_id":   "<manteion-assigned>",
  "dataset_id":      "<optional, mutually exclusive with dataset_inline>",
  "dataset_inline":  { /* inline pools, capped at 1 MiB, see Data Delivery */ },
  "vus":             50,
  "duration_s":      300,
  "persona":         "balanced",
  "meta_trace_id":   "<optional; zeus generates if absent>",
  "labels":          { "phase": "isolation-1a", "frozen_service": "productcatalog" },
  "workflow_label":  "browse"
}
```

```json
// response 202
{
  "run_id":          "...",
  "status":          "starting",
  "meta_trace_id":   "...",
  "k6_job_name":     "zeus-run-<run_id>",
  "started_at":      "..."
}

// 400 — invalid params
// 409 — run_id already exists
// 422 — data validation failed (run state: rejected)
```

`workflow_label` is injected into every request's W3C Baggage header as `atropos.workflow=<value>`. It defaults to the workflow's `name` if not set.

#### List runs for a workflow

```
GET /api/v1/workflows/{id}/runs
```

```json
// response 200 -- each element is a full run document (see "Get a run")
{ "runs": [ { "id", "workflow_id", "status", "started_at", "ended_at", "labels", ... } ] }
```

#### Get a run (cross-workflow)

```
GET /api/v1/runs/{run_id}
```

```json
// response 200
{
  "id": "...",
  "workflow_id": "...",
  "workflow_name": "browse",
  "experiment_id": "...",
  "dataset_id": "...",
  "status": "running",
  "meta_trace_id": "...",
  "workflow_label": "browse",
  "labels": { "phase": "baseline" },
  "k6_job_name": "zeus-run-<id>",
  "started_at": "...",
  "ended_at": "...",   // omitted until the run is terminal
  "reason": "..."      // omitted unless set (failure reason, "thresholds breached", ...)
}
```

The run's key is `id`. (The create response above names the same value `run_id`.) The run
document carries no stats; read them from `GET /runs/{run_id}/stats`.

#### List all runs (with filters)

```
GET /api/v1/runs?status=running&experiment_id=X&workflow_id=Y&since=<rfc3339>
```

#### Stop a running run

```
DELETE /api/v1/runs/{run_id}
```

```
204 — stopped; k6 killed.
404 — run not found
409 — run already terminal (completed | stopped | failed | rejected)
```

k6 is killed, so it writes no summary export. The stats gathered from k6's live output up to
the stop are kept: once zeus has drained k6's last output (normally well under a second),
`GET /runs/{run_id}/stats` serves them as the run's final snapshot with `"status": "stopped"`,
`"final": true`, `"source": "k6_stream"`. The run's Prometheus series keep their last values
(see "Cardinality budget"). The event stream sends that final `stats.snapshot` and then closes.

#### Streaming event tail

```
GET /api/v1/runs/{run_id}/events
```

```
200 text/event-stream (SSE)
event: run.state
data: {"run_id":"...","status":"running","detail":""}

event: stats.snapshot
data: { /* the run's stats snapshot, same document as GET /runs/{run_id}/stats */ }

: heartbeat
```

Two event types are emitted, nothing else:

- `run.state` on every status change. `detail` carries the failure reason for `failed`, and
  `killed` when a stopped run's k6 process has been reaped.
- `stats.snapshot` every 2 seconds (at most 5) while the run is in flight (`"final": false`),
  and once more with the final snapshot (`"final": true`) when the run ends.

On connect the stream first sends the run's current `run.state` and, if one exists, its latest
`stats.snapshot`. A comment line (`: heartbeat`) is sent every 10 seconds (at most 15). The stream
stays open for the whole run, however long, regardless of the server's write timeout, and zeus
closes it once the run is terminal. A client that connects to an already-terminal run gets the
state and final snapshot and then the stream closes. Events carry no `id`; `Last-Event-ID` is not
honored and a subscriber starts at the live edge. Slow subscribers may miss events (each has a
256-event buffer).

Used by manteion for live dashboards; poll `GET /runs/{run_id}/stats` for the authoritative final
numbers.

### 2. Dataset management

#### Register a dataset

```
POST /api/v1/datasets
```

```json
// request
{
  "name":   "socialnet-10k",
  "pools":  { "users": { "fields": [...], "count": 10000 } },
  "source": "upload",
  "ttl_s":  86400
}

// response 201
{
  "id": "<dataset_id>",
  "name": "socialnet-10k",
  "created_at": "...",
  "pool_stats": { "users": { "count": 0, "fields": [...] } }
}
```

`source` can be `"upload"` (NDJSON push via the upload endpoint) or `"cache_box_dump"` (future: manteion projects from atropos cache-box export). `ttl_s` of 0 means permanent. Default TTL: 24 hours.

#### Stream pool data into a dataset

```
POST /api/v1/datasets/{id}/upload
Content-Type: application/x-ndjson
```

```
{"pool":"users","rows":[{"id":"u1","email":"a@b.com","region":"us-west"},{"id":"u2",...}]}
{"pool":"products","rows":[{"id":"p1","name":"widget","price":9.99},...]}
```

```json
// response 200
{ "ingested": { "users": 10000, "products": 250 } }
// 413 — payload too large (per-chunk limit)
```

#### List datasets

```
GET /api/v1/datasets
```

```json
// response 200
{ "datasets": [ { "id", "name", "pool_stats", "created_at", "ttl_s" } ] }
```

#### Dataset metadata

```
GET /api/v1/datasets/{id}
```

```json
// response 200
{ "id", "name", "pools": {...}, "pool_stats": {...}, "size_bytes": 1024000, "ttl_s": 86400 }
```

#### Inspect sample rows

```
GET /api/v1/datasets/{id}/sample?pool=users&limit=5
```

```json
// response 200
{ "pool": "users", "rows": [ {"id":"u1","email":"a@b.com"}, ... ] }
```

#### Remove a dataset

```
DELETE /api/v1/datasets/{id}
```

```
204 — deleted
409 — in use by an active run
```

### 3. Stats and metrics

#### Per-run structured stats

```
GET /api/v1/runs/{run_id}/stats
```

```json
// response 200
{
  "run_id": "...",
  "workflow_id": "...",
  "status": "running",          // the run's status when the snapshot was taken
  "final": false,               // false = interim (run in flight); true = will not change again
  "source": "k6_stream",        // "k6_summary" | "k6_stream" (see below)
  "duration": 12000000000,      // elapsed run time so far, nanoseconds
  "iterations": 412,            // k6 iterations
  "requests_sent": 1648,        // k6 http_reqs
  "requests_ok": 1640,          // http_reqs minus k6 http_req_failed
  "requests_dropped": 0,        // k6 dropped_iterations (open-loop arrivals skipped)
  "active_vus": 50,             // k6's last vus value; 0 in final snapshots
  "latency_p50": 42000000,      // http_req_duration percentiles, nanoseconds
  "latency_p95": 180000000,
  "latency_p99": 450000000,
  "variant_distribution": null, // reserved; the k6 engine does not report variant picks
  "updated_at": "...",          // when this snapshot was taken
  "finalized_at": "..."         // final snapshots only
}

// response 200, before the run has started k6 (or for a run that never did)
{ "run_id": "...", "status": "no stats available yet" }

// 404 -- run not found
```

While the run is in flight the snapshot is **interim**: zeus aggregates k6's live JSON output as it
arrives and refreshes the snapshot every 2 seconds (at most 5). Interim latency percentiles come
from a fixed-size log-bucketed sketch and are within ~1% of the exact value.

When the run ends the snapshot becomes **final** (`"final": true`):

| Run ended as | `source` | Numbers |
|---|---|---|
| `completed` (including a threshold breach, k6 exit 99) | `k6_summary` | k6's `--summary-export`, as before. Exact percentiles. |
| `completed` but the summary export is missing or unreadable | `k6_stream` | stream totals |
| `stopped` (DELETE) | `k6_stream` | stream totals gathered up to the stop |
| `failed` | `k6_stream` | stream totals gathered up to the failure (all zeros if k6 never ran load) |

The final snapshot can trail the terminal status by a moment while zeus drains k6's last output, so
a client that wants the final numbers polls until `"final": true`.

#### Per-attack stats

```
GET /api/v1/attacks/{id}/stats
```

```json
// response 200
{
  "attack_id": "...",
  "status": "completed",
  "config": { /* AttackConfig */ },
  "result": {
    "total_requests": 30000,
    "duration_s": 300,
    "rate_actual": 100.5,
    "success": 0.998,
    "status_codes": { "200": 29950, "500": 50 },
    "latencies": { "p50_ms": 8, "p90_ms": 22, "p95_ms": 35, "p99_ms": 120, "max_ms": 800 }
  },
  "dedup": {
    "strategy": "header",
    "variants_generated": 30000,
    "collisions": 3
  }
}
```

#### Prometheus scrape endpoint

```
GET /api/v1/metrics
```

```
200 text/plain; version=0.0.4
```

See Prometheus metrics catalog below.

#### Zeus-wide summary

```
GET /api/v1/metrics/summary
```

```json
// response 200
{ "active_runs": 3, "active_attacks": 1, "datasets": 8, "rps_global": 320.5, "uptime_s": 8123 }
```

### 4. Precision attack control

These endpoints stay from the current API, extended with `experiment_id`, `meta_trace_id`, and `workflow_label`.

#### Launch an attack

```
POST /api/v1/attacks
```

```json
// request — attacker.AttackConfig
{
  "target": {
    "url":     "http://productcatalog:3550/products",
    "method":  "GET",
    "headers": { "Accept": "application/json" }
  },
  "rate":            100,
  "duration_s":      60,
  "dedup_bypass":    { "strategy": "header", "source": "X-Idempotency-Key" },
  "experiment_id":   "<manteion-assigned>",
  "meta_trace_id":   "<manteion-assigned: phase/trace id, echoed as W3C baggage>",
  "workflow_label":  "browse",

  "timeout_s":       30,
  "max_connections": 10000,
  "max_body_bytes":  -1,
  "redirects":       10
}

// response 201 (manteion's StartAttack accepts only 200/201)
{ "id": "<attack_id>", "status": "running", "started_at": "..." }
```

Field notes:

- `target` matches `attacker.TargetSpec` — `url`, `method`, optional `headers`, optional `body` (base64 in JSON since `Body` is `[]byte`). REST routes on the Go reimplementation of productcatalog are `/products`, `/products/{id}`, `/products/batch`, `/products/search`.
- `duration_s` is an integer number of seconds (matches the run-create convention).
- `dedup_bypass` is an object: `strategy` is `"header"` or `"query"`; `source` is the header name or query-param name to randomize per request. Omit `source` to use the strategy default (`X-Idempotency-Key` / `nonce`).
- `timeout_s`, `max_connections`, `max_body_bytes`, `redirects` are optional vegeta tuning knobs. Zero (or omitted) uses the vegeta default.
- `workflow_label` is injected into the vegeta targeter's W3C Baggage alongside `meta-trace-id`. **Zeus does not cross-check this label against any atropos rule registry** — a label no rule matches silently produces a no-isolation run. Manteion owns that verification.
- `experiment_id` is used as a Prometheus label on `zeus_attacks_started_total` and the series is never pruned. Manteion is expected to keep experiment-id cardinality bounded.

#### List attacks

```
GET /api/v1/attacks?status=running&experiment_id=X
```

#### Get attack status

```
GET /api/v1/attacks/{id}
```

```json
// response 200 — id is top-level (mirrors manteion's zeus.AttackInfo)
{ "id": "<attack_id>", "workload_id": "", "service": "",
  "status": "running|completed|stopped", "started_at": "...", "completed_at": "..." }
```

#### Get attack result

```
GET /api/v1/attacks/{id}/result
```

Final vegeta metrics, normalized to manteion's `AttackResultInfo` (duration in ms, latency
percentiles in microseconds). Returns **404 while the attack is still running** (no result
yet) so callers can poll; the body materializes once the attack finalizes. `service` is empty
— a single-URL attack targets a raw URL, not a named service.

```json
// response 200
{ "attack_id": "<attack_id>", "service": "", "total_requests": 6000, "duration_ms": 60000,
  "rate_actual": 100.0, "success_rate": 0.998,
  "latency_p50_us": 1200, "latency_p90_us": 2400, "latency_p95_us": 3100, "latency_p99_us": 8200,
  "throughput_rps": 99.8 }
```

#### Stop an attack

```
DELETE /api/v1/attacks/{id}
```

```
204 — stopped
```

### 5. Health

```
GET /healthz        — liveness probe (200 if process is up)
GET /readyz         — readiness (200 if dataset store, prom registry, k6 runtime ready)
GET /api/v1/status  — operator-friendly overview: runs, attacks, datasets, errors
```

## Disposition of existing legacy endpoints

| Current endpoint | Disposition | Rationale |
|---|---|---|
| `POST /api/v1/workloads` | **Removed, absorbed.** | k6 self-registration is replaced by `POST /runs`. Zeus owns runs, not k6. |
| `GET /api/v1/workloads` | **Removed, absorbed.** | Replaced by `GET /runs`. |
| `DELETE /api/v1/workloads/{id}` | **Removed, absorbed.** | Replaced by `DELETE /runs/{run_id}`. |
| `POST /api/v1/attacks` | **Stays, extended.** | Adds `experiment_id`, `meta_trace_id`, `workflow_label`; returns 201. |
| `GET /api/v1/attacks/{id}` | **Stays, extended.** | Adds `/result` and `/stats` sub-resources. |
| `DELETE /api/v1/attacks/{id}` | **Stays.** | Unchanged. |
| `POST /api/v1/policies` | **Moved to manteion.** | Policy rules evaluate metrics against thresholds — that is manteion's concern. Deletes `internal/policy/` from zeus (~200 lines). |
| `GET /api/v1/policies` | **Moved to manteion.** | Same. |
| `DELETE /api/v1/policies/{id}` | **Moved to manteion.** | Same. |
| `GET /api/v1/status` | **Stays, expanded.** | Adds dataset count, active run count, error count. |

## Two-phase vs single-phase data delivery

**Decision: both modes exist, with two-phase as the default.**

**Two-phase (default for anything manteion runs):** Register a dataset → stream data → trigger a run referencing `dataset_id`. Benefits: large datasets (100k+ rows), reuse across many runs, immutable dataset for experiment reproducibility, no request body bloat.

**Single-phase (convenience for dev-loop):** Pass `dataset_inline` in the run-create payload. Zeus computes the hash, writes it to the dataset store with short TTL (default 1 hour), assigns a synthetic `dataset_id`, and runs against it. The synthetic id is returned in the response so the caller can re-reference it.

**Hard limit on `dataset_inline`: 1 MiB.** Above that, zeus rejects with 413 and a message telling the caller to upload first.

**When to pick each:**

| Use case | Mode | Why |
|---|---|---|
| manteion orchestrating an experiment | Two-phase | Dataset is projected from cache-box; persisted across all phases of the same experiment; immutable for reproducibility. |
| Repeated baseline/isolation runs | Two-phase | Same dataset across 15+ runs; upload once, reference forever. |
| Developer debugging a new workflow locally | Single-phase | 10 users, 20 products. Two-step dance is friction for a `curl` session. |
| CI smoke test | Single-phase | Small fixture, short-lived, disposable. |
| Dataset > 1 MiB | Two-phase (mandatory) | 413 on inline. |

**Streaming ingest path.** `POST /datasets/{id}/upload` accepts NDJSON (one JSON object per line, each tagged with a `pool` key). Zeus writes chunks incrementally to its dataset store. No `Content-Length`-bounded body is required on the upload endpoint — the store accepts streaming writes and reports per-pool counts on completion.

## Prometheus metrics catalog

Zeus uses a direct `go-prometheus` registry. Not routed through the OTel collector. The OTel path stays in atropos-go SDKs for span export only.

### Counters

```
zeus_runs_started_total{workflow_id, workflow_name}
zeus_runs_completed_total{workflow_id, workflow_name, status}
    # status: completed | stopped | failed

zeus_requests_sent_total{run_id, workflow_id, step_id}      # k6 http_reqs
zeus_requests_ok_total{run_id, workflow_id, step_id}        # k6 http_req_failed == 0
zeus_requests_dropped_total{run_id, workflow_id, step_id, reason}
    # reason: dropped_iteration (k6 dropped_iterations; step_id is ""). Nonzero = the open-loop
    # generator could not offer the configured rate; the run is invalid for delta math.
zeus_iterations_total{run_id, workflow_id}                  # k6 iterations

zeus_attacks_started_total{experiment_id, dedup_strategy}
zeus_attacks_hits_total{attack_id, target_service, dedup_strategy}
zeus_attacks_misses_total{attack_id, target_service, reason}

zeus_datasets_bytes_ingested_total{dataset_id, pool}
```

### Histograms

```
zeus_request_duration_seconds{run_id, workflow_id, step_id}  # k6 http_req_duration
zeus_iteration_duration_seconds{run_id, workflow_id}         # k6 iteration_duration
zeus_attack_request_duration_seconds{attack_id, target_service}
```

### Gauges

```
zeus_active_runs{}
zeus_active_vus{run_id}                                      # k6 vus; 0 once the run ends
zeus_active_attacks{}
zeus_datasets_total{}
zeus_dataset_size_bytes{dataset_id}
```

The per-run families (those with `run_id`) are fed from k6's live JSON output while the run is in
flight, sample by sample, so they advance continuously. `step_id` is the request's k6 `name` tag,
which the engine sets to the DSL request node's `id` (the dataset fetch in k6 `setup()` is named by
its URL). There is no per-step duration or variant-pick family: k6's output carries neither.

### Cardinality budget

The danger labels are `run_id` and `step_id`.

- **`run_id`** — a run's series keep their final values after the run ends (completed, stopped or failed), so a scrape after the end still sees the full totals, and are removed when the run's record is evicted from the run store. **Zeus has no run eviction today**: run records, their snapshots and their series live until the process restarts, so `/metrics` grows by roughly 4 × steps + 4 series (plus histogram buckets) per run over a long-lived process. Final aggregate stats stay available via `GET /runs/{run_id}/stats`.
- **`step_id`** — bounded by the workflow DSL (one value per request node id). Typical workflows have fewer than 50 steps.
- **Never label by `meta_trace_id`.** It is a per-run random 32-hex and would destroy Prometheus.
- **`attack_id`** is short-lived (attacks are minutes, not hours) and bounded by the active attack set.

## `atropos.workflow` baggage wiring

This section resolves ambiguity A9 in `atropos-go/ambiguities.md`.

### The problem

Atropos-go SDKs extract `atropos.workflow` from incoming W3C Baggage on every request. Manteion uses this label to scope cache-box rules (e.g., "freeze productcatalog only for browse traffic"). But today zeus never sets it, so the label is always empty and workflow-scoped freezes are inoperable.

### The fix

Zeus populates `atropos.workflow` at the **request site** in both the k6 engine and Zeus's vegeta targeter.

**Flow for k6 traffic:**

1. `POST /workflows/{id}/runs` accepts a `workflow_label` field. If absent, defaults to the workflow's `name`.
2. Zeus launches the k6 job container with env var `ZEUS_WORKFLOW_LABEL=<value>`.
3. `runner.js` reads `ZEUS_WORKFLOW_LABEL` in init context alongside `META_TRACE_ID`.
4. The engine injects both into every request's W3C Baggage header: `meta-trace-id=<hex>,atropos.workflow=<label>`.

**Flow for Zeus attack traffic:**

1. `POST /attacks` accepts a `workflow_label` field, injected into every request's W3C Baggage as `atropos.workflow` (the key the atropos SDK matches cache-box rules against).
2. `AttackConfig` gains a `WorkflowLabel string` field.
3. `internal/attacker/attacker.go` uses `trace.InjectLabeledBaggage(header, map)` instead of today's `trace.InjectBaggageHeader(header, traceID)`.

**Helper change:**

`internal/trace/context.go` gains an `InjectLabeledBaggage` function that accepts a `map[string]string` of key-value pairs. Today's `InjectBaggageHeader(header, traceID)` becomes a thin wrapper that calls `InjectLabeledBaggage(header, {"meta-trace-id": traceID})`. The new function also appends `atropos.workflow` if present. This is a ~20-line change and future-proofs the surface for additional baggage members.

### Validation

The fix is validated when: manteion publishes a cache-box rule with `{atropos.workflow: browse}` in its label match, zeus starts both a `browse` and a `checkout` run simultaneously, and atropos freezes productcatalog only on the browse-labelled requests. Checkout traffic passes through live.
