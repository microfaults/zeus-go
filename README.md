# zeus-go

Precision load-generation and attack orchestration platform. Combines **k6 sidecars** for workflow-driven traffic with **Zeus**, a Go service that launches targeted [Vegeta](https://github.com/tsenart/vegeta) attacks against endpoints outside the normal workflow path.

## Architecture

```
┌─────────────┐     ┌─────────────┐
│  k6-browse   │     │ k6-checkout  │    k6 sidecars run DSL v2
│  (sidecar)   │     │  (sidecar)   │    tree-structured flows
└──────┬───────┘     └──────┬───────┘
       │                     │
       ▼                     ▼
┌──────────────────────────────────┐
│              Zeus                │     Go service: attack mgmt,
│   policy engine                  │     policy engine, dedup bypass
│   vegeta attacker                │
└──────────────┬───────────────────┘
               │ vegeta attacks
               ▼
┌──────────────────────────────────┐
│      Target Application          │
└──────────────────────────────────┘
```

**k6 sidecars** generate realistic user traffic through DSL v2 tree-structured flows (browse, checkout, etc.) while **Zeus** independently hammers specific endpoints with Vegeta.

All requests carry `meta-trace-id` and `atropos.workflow` via W3C Baggage headers for distributed trace correlation and rule scoping.

## Components

### Zeus (Go service)

REST API for managing attacks and policy rules.

| Endpoint | Description |
|---|---|
| `POST /api/v1/attacks` | Launch a Vegeta attack |
| `GET /api/v1/attacks/{id}` | Get attack status/metrics |
| `DELETE /api/v1/attacks/{id}` | Stop attack |
| `POST /api/v1/policies` | Register policy rule |
| `GET /api/v1/policies` | List rules |
| `DELETE /api/v1/policies/{id}` | Remove rule |
| `GET /api/v1/status` | System status |

**Policy engine** evaluates rules on a 10-second interval and auto-launches attacks when conditions are met. Rules support cooldowns to prevent duplicate triggers.

**Dedup bypass** mutates requests to defeat idempotency checks via `X-Idempotency-Key` header injection or query-param nonce.

### k6 (load generation sidecars)

A generic, config-driven k6 runner with a DSL v2 tree-walking engine. Test scenarios are defined entirely in JSON using 5 node types: `sequence`, `parallel`, `delay`, `optional`, `request`.

**Flows** (`k6/flows/`) define node trees with scoped extracts, variants, and delays:

```json
{
  "version": "2",
  "name": "online-boutique-browse",
  "targets": ["frontend", "productcatalogservice"],
  "default_delay": { "min_ms": 3000, "max_ms": 8000 },
  "root": {
    "type": "sequence",
    "children": [
      { "type": "request", "id": "homepage", "method": "GET", "path": "/" },
      {
        "type": "optional", "probability": "explore",
        "child": {
          "type": "request", "id": "view_product",
          "method": "GET", "path": "/product/{{data.products.id}}"
        }
      }
    ]
  }
}
```

**Personas** (`k6/personas/`) control user behavior via probability maps and think times:

| Persona | Behavior | Explore | Engage | Commit | Think time |
|---|---|---|---|---|---|
| `cautious` | Window-shopper | 90% | 20% | 5% | 2-5s |
| `aggressive` | Spendthrift | 50% | 80% | 70% | 0.5-1.5s |
| `balanced` | Bargain-hunter | 95% | 40% | 30% | 3-8s |

**Template expressions** resolve at runtime:

- `{{data.<collection>.<field>}}` - random item from data pool
- `{{steps.<id>.<key>}}` - extracted value from a prior step (scope-chain walk)
- `{{random_int(min,max)}}` / `{{random_choice(a,b,c)}}` - randomization
- `{{env.VAR}}` - environment variable

See `docs/workflow-dsl-v2.md` for the full DSL v2 specification.

## Quick Start

```bash
# Set the target application URL
export BASE_URL=http://frontend:8080

# Run everything
docker compose up
```

This starts:
- **zeus** on port 8080
- **k6-browse** - browsing flow with `cautious` persona (10 VUs, 5 min)
- **k6-checkout** - checkout flow with `aggressive` persona (5 VUs, 5 min)

### Configuration

Override via environment variables:

```bash
BROWSE_VUS=20 BROWSE_DURATION=10m \
CHECKOUT_VUS=10 CHECKOUT_DURATION=10m \
BASE_URL=http://my-app:8080 \
docker compose up
```

### Run Zeus standalone

```bash
go build -o zeus ./cmd/zeus
ZEUS_ADDR=:9090 ./zeus
```

## Project Structure

```
cmd/zeus/            Entry point
internal/
  api/               HTTP handlers and routing
  attacker/          Vegeta attack orchestration
  policy/            Rule engine with metric-based triggers
  workload/          In-memory workload registry
  dedup/             Idempotency bypass strategies (header, query)
  trace/             W3C Baggage header helpers
k6/
  runner.js          Generic config-driven k6 entry point
  scripts/lib/       JS modules (engine, template, zeus client, tracing)
  flows/             DSL v2 workflow definitions (JSON)
  personas/          User behavior profiles (JSON)
docs/
  workflow-dsl-v2.md DSL v2 specification
  api-contract.md    Zeus API contract
```

## Requirements

- Go 1.25+
- Docker & Docker Compose (for containerized runs)
- k6 (if running scripts outside Docker)
