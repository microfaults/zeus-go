# Legacy k6 Cleanup Spec

This document enumerates the file-level changes needed when the zeus code refactor plan executes. It is a handoff spec, not a standalone plan. Read `workflow-dsl-v2.md` and `api-contract.md` first.

## Dead files (delete outright)

These files are dead code. They are not referenced by `docker-compose.yml`, `skaffold.yaml`, or any K8s manifest. They were the pre-declarative approach: hardcoded per-service k6 scripts that the generic `runner.js` + JSON flows replaced.

| File | Status | Notes |
|---|---|---|
| `k6/scripts/boutique-browse.js` | Dead | Hardcoded browse script. docker-compose and K8s jobs use `runner.js` with `FLOW_PATH`. |
| `k6/scripts/boutique-checkout.js` | Dead | Hardcoded checkout script. Same. |
| `k6/scripts/lib/personas.js` | Likely dead | Old persona definitions (hardcoded JS objects). Personas now live in `k6/personas/*.json`. Grep for `require.*personas` or `import.*personas` before deleting — if any live code imports it, migrate first. |

## Files that migrate to v2 (rewrite, not delete)

These files exist and are used today. They get rewritten to conform to the DSL v2 schema.

| File | Action | Notes |
|---|---|---|
| `k6/flows/online-boutique/browse.json` | Rewrite to DSL v2 | Migration example is in `docs/workflow-dsl-v2.md` (section "Migration: online-boutique browse.json in v2"). |
| `k6/flows/online-boutique/checkout.json` | Rewrite to DSL v2 | Same pattern as browse: wrap required steps in `sequence`, gate optional steps with `optional`, remove `requires` and `probability` from step bodies. |
| `k6/flows/online-boutique/data.json` | Move to dataset fixture | Becomes `k6/fixtures/online-boutique/dataset.ndjson` (NDJSON format for upload via `POST /datasets/{id}/upload`). Delete the original `data.json` after the fixture is verified. |
| `k6/flows/_example/simple.json` | Rewrite or delete | If the DSB Social Network example covers the "hello-world" use case, delete this. Otherwise rewrite to v2 as a minimal 1-step health-check workflow. |

## Files that need surgery

These files get significant code changes. Listed in dependency order for implementation.

### Go backend (zeus service)

| File / Package | Change | Why |
|---|---|---|
| `cmd/archer/main.go` | Rename binary to `zeus`. Entry point gains dataset store init, prom registry init. | The service is called Zeus now, not Archer. |
| `internal/api/server.go` | Full route refactor. Drop `/workloads`, `/policies`. Add `/workflows`, `/datasets`, `/runs`, `/metrics`. See `api-contract.md`. | New control-plane surface. |
| `internal/api/` (new files) | Add `workflow_handler.go`, `dataset_handler.go`, `run_handler.go`, `stats_handler.go`. | Clean separation of concerns per endpoint group. |
| `internal/workload/` | **Retire entirely.** Replace with `internal/workflow/` (workflow registry), `internal/run/` (run lifecycle), `internal/dataset/` (dataset store). | "Workload" was a k6-centric concept. Runs, workflows, and datasets are the new first-class entities. |
| `internal/policy/` | **Retire entirely.** Move to manteion. | Policy engine (10s eval loop, rule store) is control-plane logic. Zeus should not own it. |
| `internal/attacker/attacker.go` | Extend `AttackConfig` with `ExperimentID`, `RunRef`, `WorkflowLabel`. Swap `trace.InjectBaggageHeader` for `trace.InjectLabeledBaggage`. | Resolves ambiguity A9 and ties attacks to experiments. |
| `internal/trace/context.go` | Add `InjectLabeledBaggage(header http.Header, kv map[string]string)`. Make existing `InjectBaggageHeader` a thin wrapper. | ~20-line change. Future-proofs baggage surface. |
| `internal/dedup/` | Keep as-is. | Dedup bypass strategies are still needed for precision attacks. |
| (new) `internal/stats/` | Prometheus metric registry. Register all `zeus_*` counters, histograms, gauges per catalog in `api-contract.md`. | Zeus currently has no metrics surface. |

### k6 engine (JavaScript)

| File | Change | Why |
|---|---|---|
| `k6/runner.js` | Replace `open(FLOW_PATH)` + `open(DATA_PATH)` with HTTP fetch of dataset from `ZEUS_DATASET_ENDPOINT` at `setup()`. Read `ZEUS_WORKFLOW_LABEL` and `ZEUS_RUN_ID` env vars. | Data is external; flow definition may also come from zeus API (TBD: can keep file-based for now as transition). |
| `k6/scripts/lib/engine.js` | Full rewrite. Replace flat step list + topoSort with tree walker. Implement `sequence`, `parallel` (async), `delay`, `optional`, `request` node executors. Implement scoped extract resolution. Implement variant sampling. | This is the biggest change. ~300 lines of current engine, rewritten to ~500-600. |
| `k6/scripts/lib/template.js` | Extend with dot-path patching for variant `set` maps. Add `jsonpath:` resolution in extract values. Rename `pool.*` to `data.*`. Keep `random_int`, `random_choice`, `env`, `steps` expressions. | Moderate change. Template core is sound; variants and jsonpath are additive. |
| `k6/scripts/lib/archer.js` | Rename to `zeus.js`. Update registration endpoint from `/api/v1/workloads` to nothing (zeus owns the run; k6 does not self-register). Keep health-check polling for init container. | k6 no longer registers itself as a workload. |
| `k6/scripts/lib/tracing.js` | Add `atropos.workflow` to the baggage header alongside `meta-trace-id`. | One-line addition to the baggage construction. |

### K8s manifests and deployment

| File | Change | Why |
|---|---|---|
| `kubernetes-manifests/archer.yaml` | Rename to `zeus.yaml`. Update container name, image name, labels. | Binary and service are renamed. |
| `kubernetes-manifests/k6-browse.yaml` | Add env vars: `ZEUS_DATASET_ENDPOINT`, `ZEUS_WORKFLOW_LABEL`, `ZEUS_RUN_ID`. Remove `FLOW_PATH` and `DATA_PATH` if dataset fetch replaces file-based. | k6 pulls data from zeus at setup(). |
| `kubernetes-manifests/k6-checkout.yaml` | Same as browse. | Same. |
| `docker-compose.yml` | Update service name from `archer` to `zeus`. Update k6 env vars. | Consistency. |
| `Dockerfile` | Update binary name from `archer` to `zeus`. | Consistency. |
| `skaffold.yaml` | Update artifact and deploy names. | Consistency. |

## Sequencing

Recommended order for the code refactor:

1. **Go backend API + dataset store + prom metrics.** Rip out `/workloads` and `/policies`, add `/workflows`, `/datasets`, `/runs`, `/metrics`. Ship `internal/workflow`, `internal/dataset`, `internal/run`, `internal/stats`. Verify with curl and a simple workflow registration. This is the foundation.
2. **Baggage wiring.** Add `InjectLabeledBaggage` to `internal/trace/context.go`, extend `AttackConfig`. Verify with a test attack that carries `atropos.workflow` in its headers.
3. **k6 engine rewrite.** Port `engine.js` to DSL v2. Port `template.js` for variants and `jsonpath:`. Verify by running the migrated online-boutique browse.json v2 against the boutique frontend.
4. **Migrate online-boutique flows.** Rewrite `browse.json` and `checkout.json` to v2. Convert `data.json` to `dataset.ndjson`. Run a full k6 workload against a live boutique cluster.
5. **Delete dead files.** Remove `boutique-browse.js`, `boutique-checkout.js`, `personas.js`. Rename `archer.js` → `zeus.js`. Rename K8s manifests and Dockerfile. This is last because it is irreversible and should only happen after everything else works.

## Risk notes

- **K8s manifests already reference `runner.js`.** The generic runner entry point is the live path. No K8s manifest references the legacy scripts. The risk of the dead file deletion is minimal.
- **`internal/policy/` retirement.** The policy engine's 10-second eval loop is the only automated attack trigger today. Removing it means attacks must be triggered explicitly (via `POST /attacks` from manteion or CLI). This is intentional — automated trigger logic moves to manteion — but means the system loses automated attack triggering until manteion ships. Document this regression clearly in the PR.
- **k6 engine rewrite scope.** The current engine is ~310 lines. The rewrite will be ~500-600 lines. Parallel execution within a VU requires careful use of k6's async primitives (`http.asyncRequest` → `Promise.all`). Write integration tests against a mock HTTP server before wiring to a real target.
- **Renaming archer → zeus.** Touch every manifest, every import path, every log message. Do it in one commit to keep `git blame` clean.
