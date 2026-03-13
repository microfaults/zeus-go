# zeus-go

Precision load-generation and attack orchestration platform. Combines **k6 sidecars** for workflow-driven traffic with **Archer**, a Go service that launches targeted [Vegeta](https://github.com/tsenart/vegeta) attacks against endpoints outside the normal workflow path.

## Architecture

```
┌─────────────┐     ┌─────────────┐
│  k6-browse   │     │ k6-checkout  │    k6 sidecars run declarative
│  (sidecar)   │     │  (sidecar)   │    flows with persona-based
└──────┬───────┘     └──────┬───────┘    behavior profiles
       │  register/          │
       │  deregister         │
       ▼                     ▼
┌──────────────────────────────────┐
│            Archer                │     Go service: attack mgmt,
│   workload registry              │     policy engine, dedup bypass
│   policy engine                  │
│   vegeta attacker                │
└──────────────┬───────────────────┘
               │ vegeta attacks
               ▼
┌──────────────────────────────────┐
│      Target Application          │
└──────────────────────────────────┘
```

**k6 sidecars** generate realistic user traffic through JSON-defined flows (browse, checkout, etc.) while **Archer** independently hammers specific endpoints with Vegeta — useful for stress-testing services that sit outside the normal user workflow or for applying focused load alongside organic-looking traffic.

All requests carry a shared `meta-trace-id` via W3C Baggage headers for distributed trace correlation.

## Components

### Archer (Go service)

REST API for managing workloads, attacks, and policy rules.

| Endpoint | Description |
|---|---|
| `POST /api/v1/workloads` | Register a k6 workload |
| `GET /api/v1/workloads` | List active workloads |
| `DELETE /api/v1/workloads/{id}` | Deregister workload |
| `POST /api/v1/attacks` | Launch a Vegeta attack |
| `GET /api/v1/attacks/{id}` | Get attack status/metrics |
| `DELETE /api/v1/attacks/{id}` | Stop attack |
| `POST /api/v1/policies` | Register policy rule |
| `GET /api/v1/policies` | List rules |
| `DELETE /api/v1/policies/{id}` | Remove rule |
| `GET /api/v1/status` | System status |

**Policy engine** evaluates rules on a 10-second interval and auto-launches attacks when conditions are met (e.g., `active_workloads > 0`). Rules support cooldowns to prevent duplicate triggers.

**Dedup bypass** mutates requests to defeat idempotency checks — via `X-Idempotency-Key` header injection or query-param nonce — so Vegeta attacks aren't silently deduplicated by the target.

### k6 (load generation sidecars)

A generic, config-driven k6 runner that eliminates per-service JavaScript. Test scenarios are defined entirely in JSON.

**Flows** (`k6/flows/`) define step DAGs with dependencies, probabilities, and data extraction:

```json
{
  "steps": [
    { "name": "homepage", "method": "GET", "url": "{{env.BASE_URL}}/" },
    {
      "name": "view_product",
      "method": "GET",
      "url": "{{env.BASE_URL}}/product/{{pool.products.id}}",
      "requires": ["homepage"],
      "probability": "explore"
    }
  ]
}
```

**Personas** (`k6/personas/`) control user behavior via probability maps and think times:

| Persona | Behavior | Explore | Engage | Commit | Think time |
|---|---|---|---|---|---|
| `cautious` | Window-shopper | 90% | 20% | 5% | 2–5s |
| `aggressive` | Spendthrift | 50% | 80% | 70% | 0.5–1.5s |
| `balanced` | Bargain-hunter | 95% | 40% | 30% | 3–8s |

**Template expressions** resolve at runtime:

- `{{pool.<collection>.<field>}}` — random item from data pool
- `{{steps.<step>.<key>}}` — extracted value from a prior step
- `{{random_int(min,max)}}` / `{{random_choice(a,b,c)}}` — randomization
- `{{env.VAR}}` — environment variable

## Quick Start

```bash
# Set the target application URL
export BASE_URL=http://frontend:8080

# Run everything
docker compose up
```

This starts:
- **archer** on port 8080
- **k6-browse** — browsing flow with `cautious` persona (10 VUs, 5 min)
- **k6-checkout** — checkout flow with `aggressive` persona (5 VUs, 5 min)

### Configuration

Override via environment variables:

```bash
BROWSE_VUS=20 BROWSE_DURATION=10m \
CHECKOUT_VUS=10 CHECKOUT_DURATION=10m \
BASE_URL=http://my-app:8080 \
docker compose up
```

### Run Archer standalone

```bash
go build -o archer ./cmd/archer
ARCHER_ADDR=:9090 ./archer
```

## Project Structure

```
cmd/archer/          Entry point
internal/
  api/               HTTP handlers and routing
  attacker/          Vegeta attack orchestration
  policy/            Rule engine with metric-based triggers
  workload/          In-memory workload registry
  dedup/             Idempotency bypass strategies (header, query)
  trace/             W3C Baggage header helpers
k6/
  runner.js          Generic config-driven k6 entry point
  scripts/lib/       JS modules (engine, templates, tracing, archer client)
  flows/             Declarative test scenarios (JSON)
  personas/          User behavior profiles (JSON)
```

## Requirements

- Go 1.25+
- Docker & Docker Compose (for containerized runs)
- k6 (if running scripts outside Docker)
