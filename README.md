# zeus-go

Execution plane of the faults-lab performance-attribution instrument. Zeus does exactly what the
control plane (manteion) tells it to: it executes **workflow runs** by launching
[k6](https://k6.io) as a supervised subprocess per run, and fires **additive vegeta attacks** at
specific endpoints. It keeps no durable state — a run's truth lives in memory for the life of the
process and in the stats snapshot parsed from k6's summary.

## How a run works

1. manteion registers a DSL-v2 workflow document (`POST /api/v1/workflows`; zeus keys its store on
   the document's own `id`, minting one only if absent — manteion stamps its workflow id in at
   materialize time so the two planes agree).
2. `POST /api/v1/workflows/{id}/runs` creates the run and launches k6: the document is staged under
   `$ZEUS_K6_DIR/flows/zeus-runs/<run_id>.json`, k6 executes `runner.js` with the flow, persona,
   VUs/duration, dataset endpoint, and correlation ids passed as `-e` environment.
3. The run walks a state machine: `starting → validating → running → completing →
   completed` (or `failed`; `stopped` via DELETE; `rejected` at create for invalid documents).
   Every transition publishes a `run.state` event on `GET /api/v1/runs/{id}/events` (SSE).
4. On exit, zeus parses k6's `--summary-export` into the snapshot store —
   `GET /api/v1/runs/{id}/stats` serves p50/p90/p95/p99, rates, and request counts.
5. A **threshold breach is still a completed run**: k6 exit code 99 (any threshold, including the
   engine's built-in `dropped_iterations == 0` open-loop guard) completes with the reason
   `thresholds breached` recorded and the stats preserved. Load-quality judgment belongs to
   manteion's verdict, not to zeus. Any other nonzero exit is `failed` with the output tail as the
   reason.
6. A supervision deadline (run duration + grace, capped) guarantees a wedged k6 cannot leave a run
   `running` forever — the watchdog kills it and marks the run `failed` with a deadline reason.

**Restart amnesia is by design**: runs, attacks, and datasets are in-memory. After a zeus restart,
`GET /api/v1/runs/{id}` for a pre-restart run returns 404 — pollers must treat that as terminal,
not transient.

## The k6 engine

`k6/runner.js` + `k6/scripts/lib/` execute tree-structured DSL-v2 flows (`sequence`, `request`,
`delay`, `optional`, `repeat`, `if` — see [`docs/workflow-dsl-v2.md`](docs/workflow-dsl-v2.md) for
the authoritative node set, extract scoping, and variant semantics). Load is **open-loop**
(`constant-arrival-rate`): offered RPS is held constant across phases so a frozen (faster) service
cannot implicitly inflate load on the rest of the mesh. Personas gate optional branches by
probability. Datasets referenced by a flow's data schema are fetched once at k6 setup from
`GET /api/v1/datasets/{id}/content` (flat `{pool: [rows]}`).

Every request carries W3C Baggage: `meta-trace-id` (from `ZEUS_META_TRACE_ID`, i.e. the exact id
zeus returned to manteion at run create) and `atropos.workflow` for rule scoping.

## HTTP surface

| Group | Endpoints |
|---|---|
| Workflows | `POST/GET /api/v1/workflows`, `GET/DELETE /{id}`, `POST /workflows/validate` (stateless), `POST /{id}/validate` |
| Runs | `POST/GET /workflows/{id}/runs`, `GET /api/v1/runs`, `GET/DELETE /runs/{run_id}`, `GET /runs/{run_id}/events` (SSE), `GET /runs/{run_id}/stats` |
| Datasets | `POST/GET /api/v1/datasets`, `GET/DELETE /{id}`, `POST /{id}/upload` (NDJSON), `GET /{id}/sample`, `GET /{id}/content` |
| Attacks | `POST/GET /api/v1/attacks`, `GET/DELETE /{id}`, `GET /{id}/result`, `GET /{id}/stats` |
| Ops | `GET /healthz`, `GET /readyz`, `GET /api/v1/status`, `GET /api/v1/metrics` (Prometheus), `GET /api/v1/metrics/summary` |

[`docs/api-contract.md`](docs/api-contract.md) describes the contract; where it and the code
disagree, the code is authoritative.

## Environment

| Variable | Default | Purpose |
|---|---|---|
| `ZEUS_ADDR` | `:8080` | Listen address |
| `ZEUS_SELF_URL` | `http://localhost` + addr | Base URL the k6 subprocess uses to fetch dataset content |
| `ZEUS_K6_BIN` | `k6` | k6 binary |
| `ZEUS_K6_DIR` | `./k6` | k6 assets root (runner.js, flows, personas, staged run documents) |

Per-run knobs (persona, VUs, duration, base URL, dataset, workflow label, meta trace id) arrive in
the run-create request and are passed to k6 as environment, not read from zeus's own env.

## Deploy

The Dockerfile bundles the `grafana/k6:0.49.0` binary plus the `k6/` assets, so the container is
self-sufficient. Production runs as a single SHA-pinned container on VM2 (nerdctl); manteion
reaches it via `ZEUS_URL`.

## Development

```bash
go build ./... && go vet ./...
go test -race ./...
./scripts/smoke.sh          # contract smoke against a running zeus
go run ./cmd/zeus           # needs k6 on PATH for real runs
```

## Layout

```
cmd/zeus/            entry point + config
cmd/gen-dataset/     synthetic dataset generator (NDJSON)
internal/api/        handlers + routing
internal/run/        run store, state machine, k6 subprocess launcher
internal/attacker/   vegeta attack manager
internal/workflow/   DSL v2 store + schema validation
internal/dataset/    TTL dataset store + NDJSON ingest
internal/stats/      Prometheus registry + run snapshots
internal/sse/        run.state event broker
internal/dedup/      idempotency-bypass request mutators
k6/                  runner.js, engine, flows, personas
```

Internal research project of the UCSC Faults Lab (Peter Alvaro's group). Not licensed for external use.
