# Zeus Control-Plane API Contract

Zeus is the execution plane for faults-lab. Manteion issues commands to it. This document is the integration contract between the two.

All paths are under `/api/v1`. Request and response bodies are JSON unless noted. Timestamps are RFC 3339. Status codes follow REST conventions.

## Run lifecycle state machine

A run has one of six states:

```
POST /runs
    → starting     (k6 container created, dataset binding queued)
    → validating    (data schema validation running)
    → running       (k6 executing iterations)
    → completing    (k6 finished, stats flushing)
    → completed     (terminal, stats available)
       | stopped    (terminal, user cancelled via DELETE)
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
// response 200
{ "runs": [ { "run_id", "status", "started_at", "ended_at", "labels" } ] }
```

#### Get a run (cross-workflow)

```
GET /api/v1/runs/{run_id}
```

```json
// response 200
{
  "run_id": "...",
  "workflow_id": "...",
  "experiment_id": "...",
  "status": "running",
  "started_at": "...",
  "ended_at": null,
  "meta_trace_id": "...",
  "labels": { "phase": "baseline" },
  "k6_job_name": "...",
  "summary": { /* embedded stats, same shape as /runs/{id}/stats */ }
}
```

#### List all runs (with filters)

```
GET /api/v1/runs?status=running&experiment_id=X&workflow_id=Y&since=<rfc3339>
```

#### Stop a running run

```
DELETE /api/v1/runs/{run_id}
```

```
204 — stopped; k6 job terminated, final stats flushed.
```

#### Streaming event tail

```
GET /api/v1/runs/{run_id}/events
```

```
200 text/event-stream (SSE)
event: step.ok
data: {"step_id":"compose-post","latency_ms":42,"status":200,"variant_index":1}

event: step.drop
data: {"step_id":"compose-post","reason":"expectation","expected":[200],"got":500}

event: iteration.done
data: {"iteration":4001,"status":"ok","duration_ms":1240}

event: stats.snapshot
data: { /* periodic stats summary */ }
```

Used by manteion for live dashboards, not aggregated stats collection.

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
  "status": "completed",
  "started_at": "...",
  "ended_at": "...",
  "requests": {
    "sent": 12345,
    "dropped_expectation": 12,
    "dropped_timeout": 5,
    "by_step": {
      "compose-post": { "sent": 200, "ok": 198, "dropped": 2 }
    },
    "by_variant": {
      "compose-post": { "0": 120, "1": 60, "2": 20 }
    },
    "by_status": { "200": 12000, "302": 250, "500": 95 }
  },
  "latencies": {
    "all": { "p50_ms": 42, "p90_ms": 110, "p95_ms": 180, "p99_ms": 450, "max_ms": 1200 },
    "by_step": { "homepage": { "p50_ms": 10, "p99_ms": 55 } }
  },
  "iterations": { "completed": 4000, "failed": 14, "in_progress": 50 },
  "workflow_context": { "active_vus": 50, "estimated_rps": 100 }
}
```

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

These endpoints stay from the current Archer API, extended with `experiment_id` and `run_ref`.

#### Launch an attack

```
POST /api/v1/attacks
```

```json
// request — extends current AttackConfig with:
{
  "target":          "http://productcatalog:3550/ListProducts",
  "rate":            100,
  "duration_s":      60,
  "method":          "POST",
  "body":            "{}",
  "headers":         { "Content-Type": "application/json" },
  "dedup_bypass":    "header",
  "experiment_id":   "<manteion-assigned>",
  "run_ref":         "<optional: tie this attack to a concurrent workflow run>",
  "workflow_label":  "browse"
}

// response 202
{ "id": "<attack_id>", "status": "running", "started_at": "..." }
```

`workflow_label` is injected into the vegeta targeter's W3C Baggage alongside `meta-trace-id`.

#### List attacks

```
GET /api/v1/attacks?status=running&experiment_id=X&run_ref=Y
```

#### Get attack status

```
GET /api/v1/attacks/{id}
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

## Disposition of existing Archer endpoints

| Current endpoint | Disposition | Rationale |
|---|---|---|
| `POST /api/v1/workloads` | **Removed, absorbed.** | k6 self-registration is replaced by `POST /runs`. Zeus owns runs, not k6. |
| `GET /api/v1/workloads` | **Removed, absorbed.** | Replaced by `GET /runs`. |
| `DELETE /api/v1/workloads/{id}` | **Removed, absorbed.** | Replaced by `DELETE /runs/{run_id}`. |
| `POST /api/v1/attacks` | **Stays, extended.** | Adds `experiment_id`, `run_ref`, `workflow_label`. |
| `GET /api/v1/attacks/{id}` | **Stays, extended.** | Adds `/stats` sub-resource. |
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
    # status: completed | stopped | failed | rejected

zeus_requests_sent_total{run_id, workflow_id, step_id, target_service, method}
zeus_requests_ok_total{run_id, workflow_id, step_id, target_service, status_code}
zeus_requests_dropped_total{run_id, workflow_id, step_id, drop_reason}
    # drop_reason: expectation | timeout | error | variant_skip

zeus_variant_picks_total{run_id, workflow_id, step_id, variant_index}
zeus_iterations_total{run_id, workflow_id, status}
    # status: ok | partial | failed

zeus_attacks_started_total{experiment_id, dedup_strategy}
zeus_attacks_hits_total{attack_id, target_service, dedup_strategy}
zeus_attacks_misses_total{attack_id, target_service, reason}

zeus_datasets_bytes_ingested_total{dataset_id, pool}
```

### Histograms

```
zeus_request_duration_seconds{run_id, workflow_id, step_id, target_service, status_code}
zeus_step_duration_seconds{run_id, workflow_id, step_id}
zeus_iteration_duration_seconds{run_id, workflow_id}
zeus_attack_request_duration_seconds{attack_id, target_service}
```

### Gauges

```
zeus_active_runs{workflow_id}
zeus_active_vus{run_id, workflow_id}
zeus_active_attacks{}
zeus_datasets_total{}
zeus_dataset_size_bytes{dataset_id}
```

### Cardinality budget

The danger labels are `run_id`, `step_id`, `variant_index`, and `status_code`.

- **`run_id`** — high cardinality over time, bounded at any instant by the active run set. Counters and histograms keep `run_id` only while the run is active. On run completion (`DELETE /runs/{run_id}`), zeus calls `DeleteLabelValues` to prune the series. Post-completion, aggregate stats are accessible via `GET /runs/{run_id}/stats` (read from a finalized stats snapshot), not from Prometheus.
- **`step_id`** — bounded by the workflow DSL. Typical workflows have fewer than 50 steps. Cap at 100 per workflow; reject registrations above that at schema validation time.
- **`variant_index`** — bounded by the longest variants list. Cap at 16. Document it.
- **`status_code`** — naturally bounded (~20 values in practice).
- **Never label by `meta_trace_id`.** It is a per-run random 32-hex and would destroy Prometheus.
- **`attack_id`** is short-lived (attacks are minutes, not hours) and bounded by the active attack set.

## `atropos.workflow` baggage wiring

This section resolves ambiguity A9 in `atropos-go/ambiguities.md`.

### The problem

Atropos-go SDKs extract `atropos.workflow` from incoming W3C Baggage on every request. Manteion uses this label to scope cache-box rules (e.g., "freeze productcatalog only for browse traffic"). But today zeus never sets it, so the label is always empty and workflow-scoped freezes are inoperable.

### The fix

Zeus populates `atropos.workflow` at the **request site** in both the k6 engine and Archer's vegeta targeter.

**Flow for k6 traffic:**

1. `POST /workflows/{id}/runs` accepts a `workflow_label` field. If absent, defaults to the workflow's `name`.
2. Zeus launches the k6 job container with env var `ZEUS_WORKFLOW_LABEL=<value>`.
3. `runner.js` reads `ZEUS_WORKFLOW_LABEL` in init context alongside `META_TRACE_ID`.
4. The engine injects both into every request's W3C Baggage header: `meta-trace-id=<hex>,atropos.workflow=<label>`.

**Flow for Archer attack traffic:**

1. `POST /attacks` accepts a `workflow_label` field. If absent and `run_ref` is set, defaults to the linked run's `workflow_label`.
2. `AttackConfig` gains a `WorkflowLabel string` field.
3. `internal/attacker/attacker.go` uses `trace.InjectLabeledBaggage(header, map)` instead of today's `trace.InjectBaggageHeader(header, traceID)`.

**Helper change:**

`internal/trace/context.go` gains an `InjectLabeledBaggage` function that accepts a `map[string]string` of key-value pairs. Today's `InjectBaggageHeader(header, traceID)` becomes a thin wrapper that calls `InjectLabeledBaggage(header, {"meta-trace-id": traceID})`. The new function also appends `atropos.workflow` if present. This is a ~20-line change and future-proofs the surface for additional baggage members.

### Validation

The fix is validated when: manteion publishes a cache-box rule with `{atropos.workflow: browse}` in its label match, zeus starts both a `browse` and a `checkout` run simultaneously, and atropos freezes productcatalog only on the browse-labelled requests. Checkout traffic passes through live.
