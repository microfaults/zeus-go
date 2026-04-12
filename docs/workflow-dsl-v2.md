# Zeus Workflow DSL v2

This document specifies the refined JSON schema that replaces the existing `k6/flows/*.json` format. It is the contract that the rewritten `k6/scripts/lib/engine.js` and its helpers will implement against.

## Motivation

Four limits of the v1 DSL drive this rewrite. They are baked into the schema, not the runtime.

1. **Composition is topological, not lexical.** Dependencies in v1 are expressed as `requires: "homepage"` and resolved by `topoSort`. There is no way to say "these four steps must all finish before the next one runs" or "run these two branches concurrently inside one VU iteration." Parallelism in v1 is emergent across VUs, never within a session.
2. **Inter-step timing is a single global knob.** The v1 engine sleeps `persona.thinkTime` between every step. A workflow cannot say "immediately after login, hit the timeline (no think time)" and "wait 2-4 seconds before composing a post."
3. **Body variants are impossible within a step.** A v1 step has exactly one body. Expressing "60% us-west, 30% eu, 10% ap" forces duplicating the step three times, which breaks `requires` chains and fragments k6 metric names.
4. **Every step is probabilistic.** `shouldExecute` makes `probability == undefined` the "always run" case, but the convention in the existing online-boutique flows is to put `"probability": "explore"` on nearly every step. Real required steps end up at the mercy of persona numbers. Balanced persona's `explore = 0.95` means 5% of browse sessions skip viewing any product, which is noise, not a workload shape.

## Design principles

- **Tree, not list.** A workflow is a node tree. Every node has a `type` discriminator. Leaf nodes are requests; composite nodes are `sequence`, `parallel`, or `delay`. The topological sort is gone.
- **Required by default.** A step runs unless something explicitly says otherwise. The v1 `probability` field moves to an `optional` wrapper that the author opts into.
- **Variants are first-class inside a request.** A request node may carry a list of `variants`, each with a weight and a sparse dot-path patch. The engine picks one variant per iteration.
- **Delay is a node.** Inter-step timing is a `delay` node the author places wherever they want. The workflow's `default_delay` remains as the default that fires after each request, and authors can override it with per-request `before_delay` / `after_delay` or by inserting an explicit `delay` node between children.
- **Extract scope follows the tree.** Extracts do not live in a flat global map. Siblings of a sequence see earlier siblings' extracts. Parallel branches run in independent scopes and merge on completion. An optional child is transparent to its parent.
- **Data is external.** No `open()` on static files. Workflows declare a `data_schema.pools` block. Zeus pulls the dataset from its dataset store and hands it to k6 at `setup()` time over HTTP.

## JSON schema

```json
{
  "$schema": "https://faults-lab.dev/zeus/workflow.schema.json",
  "version": "2",

  "name": "<workflow name>",                          // REQUIRED, unique per tenant
  "description": "<short sentence>",
  "targets": ["<service>", ...],                      // REQUIRED; used for rule scoping
  "base_url": "http://frontend:8080",                 // default: env BASE_URL

  "estimated_rps_per_vu": 3,                          // hint to manteion for VU math
  "thresholds": {                                     // k6-style thresholds
    "http_req_failed":   ["rate<0.1"],
    "http_req_duration": ["p(95)<2000"]
  },

  "default_delay": { "min_ms": 300, "max_ms": 900 },  // inserted after each request
                                                      // unless an explicit delay follows
                                                      // or a per-request after_delay overrides

  "data_schema": {                                    // REQUIRED when any {{data.*}} expression
    "pools": {                                        // is used anywhere in the tree
      "users":    { "fields": ["id", "email", "region"], "min_size": 100 },
      "products": { "fields": ["id", "name", "price"],   "min_size": 50  },
      "posts":    { "fields": ["id", "body"],            "min_size": 200, "optional": true }
    }
  },

  "root": { /* composite or leaf node, see node types below */ }  // REQUIRED
}
```

## Node type reference

Every node anywhere in the tree has a `type` discriminator. The five types are `sequence`, `parallel`, `delay`, `optional`, and `request`.

### `sequence`

Children run in order. Later children can read extracts produced by earlier children via `{{steps.<id>.*}}`.

```json
{
  "type": "sequence",
  "id": "session",                                    // optional; auto-assigned if absent
  "children": [ <node>, <node>, ... ]                 // REQUIRED
}
```

A sequence completes when all children complete (or when one of them fails hard and error propagation is enabled). Child failures short-circuit subsequent siblings — the engine does not run later children in a sequence after an earlier child fails.

### `parallel`

Children run concurrently within a single VU iteration via k6 async primitives (`http.asyncRequest` under the hood). The parallel node returns only when the `wait` policy is satisfied.

```json
{
  "type": "parallel",
  "id": "fan-out",
  "wait": "all",                                      // "all" | "any" | { "n_of_m": 3 }
  "children": [ <node>, <node>, ... ]                 // REQUIRED
}
```

`wait` values:

- `"all"` — wait for every child to complete. Default.
- `"any"` — return as soon as any one child completes. The other children are allowed to continue in the background and their results are discarded. Useful for read-through fan-out when you only need a fast path.
- `{"n_of_m": K}` — return after K children have completed.

Parallel children run in **independent scopes**. They cannot see each other's extracts. When the parallel completes, each child's extracts are merged into the parallel's parent scope under the child's `id` (so later siblings of the parallel can read them as `{{steps.follow-a.status}}`). Siblings inside a parallel must not reference each other — if a step genuinely depends on another's output, wrap them in a `sequence` nested inside the parallel.

### `delay`

A sleep node. Two forms: explicit millisecond range or persona-key lookup.

```json
{ "type": "delay", "min_ms": 500, "max_ms": 1500 }
```

```json
{ "type": "delay", "persona_key": "think" }          // looked up in persona.delays
```

`delay` is a first-class node because inter-step timing is workflow-meaningful, not persona-meaningful. Persona think-time remains as the workflow's `default_delay` fallback; an explicit `delay` node overrides it.

### `optional`

Wraps a child with a probability gate. This is where all probabilistic behavior lives in v2.

```json
{
  "type": "optional",
  "probability": 0.3,                                 // number OR persona-key string
  "child": <node>                                     // REQUIRED; single child
}
```

`probability` is either a numeric literal in `[0, 1]` or a string that looks up a persona key (e.g., `"explore"` → `persona.probabilities.explore`). The engine evaluates the gate once at the optional node's execution time. If the gate fires, the child runs in the optional's parent scope (extracts bubble up). If it does not fire, the child is skipped and no extracts are produced.

`optional` is transparent to scope resolution: `{{steps.<child_id>.*}}` works the same whether the child ran inside or outside an `optional` wrapper, subject to "was it skipped?" — a skipped child produces no entry, and templates referencing it emit an `atropos.workflow.template.unresolved` span event.

### `request`

The leaf. This is what makes HTTP calls.

```json
{
  "type": "request",
  "id": "compose-post",                               // REQUIRED if referenced in {{steps.<id>.*}}
  "method": "POST",                                   // REQUIRED
  "path": "/wrk2-api/post/compose",                   // REQUIRED
  "headers": { "Content-Type": "application/json" },
  "body": {                                           // default body; variants patch on top
    "username":   "{{data.users.username}}",
    "user_id":    "{{data.users.id}}",
    "text":       "Hello from {{data.users.region}}",
    "media_ids":  [],
    "post_type":  0
  },

  "variants": [                                       // optional; engine picks one per iteration
    { "weight": 60, "set": { "body.region": "us-west" } },
    { "weight": 30, "set": { "body.region": "eu-west" } },
    { "weight": 10, "set": { "body.region": "ap-south", "body.post_type": 2 } }
  ],

  "expect": { "status": [200, 201] },                 // status assertion; extensible later
  "extract": {                                        // visible to later siblings of enclosing
    "post_id": "jsonpath:$.post_id"                   // sequence via {{steps.<id>.<key>}}
  },

  "before_delay": null,                               // override: skip default_delay BEFORE
  "after_delay":  { "min_ms": 2000, "max_ms": 4000 }, // override: wait 2-4s AFTER
  "timeout_ms": 10000
}
```

Field notes:

- **`method`** — any HTTP method. Form-encoded POST bodies and JSON bodies are both supported; the engine picks the encoding based on the `Content-Type` header (matching v1 behavior added in commit `cdbb537`).
- **`body`** — resolved through the template expression evaluator once per iteration. Variants are applied before template resolution.
- **`expect`** — `{"status": [...]}` for now. The object form exists so future extensions (`body_contains`, `header_matches`) can slot in without another schema break.
- **`extract`** — a map from extracted variable name to an expression. The right-hand side may be a template (`"{{data.users.id}}"`, used in v1) or a `jsonpath:` prefix (new in v2) that queries the response body. Expressions that cannot be resolved become `undefined` and emit a span event rather than throwing.
- **`before_delay` / `after_delay`** — each takes a `{min_ms, max_ms}` object, a `{persona_key}` object, or `null` to explicitly suppress the workflow's `default_delay` at that request. Not setting them inherits `default_delay` after the request.
- **`timeout_ms`** — per-request request timeout. k6 does not enforce a global default; this is the request-level override.

## Variant semantics

A `variants` list attached to a `request` node defines a weighted discrete distribution. The engine draws exactly one variant per request execution, normalizes the weights (they do not need to sum to 100), and applies the chosen variant's `set` map to the request node.

The `set` map is a **sparse path-keyed patch**. Keys are dot-paths rooted at the request node. Legal key roots are:

- `body.<path>` — writes into the request body at the given nested path. Creates intermediate objects as needed.
- `headers.<name>` — writes an HTTP header.
- `path` — overrides the request path entirely.
- `method` — overrides the method (rare; possible).
- `data.<pool>.<field>_override` — writes into the per-iteration data scope, making the value visible to `{{data.*}}` expressions in the base body.

The last form is how body variants like "60% us-west" are typically expressed: the variant writes a `data.users.region_override` into the iteration's data scope, and the base body reads `{{data.users.region_override}}`. This keeps the base body unchanged across variants and is easier to audit than three parallel copies of the same request.

Resolution order per request execution:

1. Pick a variant (or skip if none are listed).
2. Apply the variant's `set` patches to a working copy of the request node.
3. Resolve template expressions in the working copy (`{{data.*}}`, `{{steps.*}}`, `{{random_int}}`, etc.).
4. Serialize the body and send.
5. On response, run `expect` and `extract` against the response.
6. Merge extracts into the enclosing sequence scope under the request's `id`.

Weights are integers or floats, normalized per draw. A variants list with `[{weight: 1}, {weight: 1}]` is a 50/50 coin flip. A variants list with `[{weight: 60}, {weight: 30}, {weight: 10}]` is a three-way split at 60/30/10. If every weight is zero, the engine treats the list as empty and uses the base body unmodified.

Emitted metric: each variant pick increments `zeus_variant_picks_total{run_id, workflow_id, step_id, variant_index}` so manteion can verify the actual mix observed matches the requested weights (useful when a `set` key collides with a template resolution failure and the engine silently degrades).

## Extract scope rules

Extracts do not live in a flat global map. Each node in the tree has an associated scope, and scopes nest according to the tree structure. Resolution of `{{steps.<id>.<key>}}` walks **up** the scope chain from the current node; the first match wins. This section has three worked examples showing the rules in action.

### Scope kinds

- **Root scope** — one per VU iteration. Holds `data` (the iteration's data pool binding), `env`, and the baggage identity (`meta_trace_id`, `workflow_label`). Root is always the starting scope for any resolution walk.
- **Sequence scope** — child of the sequence's parent scope. Children write their extracts into the sequence scope in execution order. Later children read earlier children's extracts through the sequence scope. On successful completion the sequence scope merges into its parent under the sequence's `id`.
- **Parallel scope** — each parallel child starts a fresh child scope at the moment it is launched. Children cannot see each other's extracts, because they have not all finished when any one of them needs to resolve a template. On completion, each child's extracts merge into the parallel's parent scope under the child's `id`, indexed under the parallel's `id` if the parallel has one.
- **Optional scope** — transparent. The optional node itself has no scope; its child runs in the optional's parent scope, so the child's extracts are visible to later siblings of the `optional` node keyed by the child's `id`.
- **Delay** — no scope.

### Example 1: Linear sequence (equivalent to v1)

```json
{
  "type": "sequence",
  "id": "session",
  "children": [
    { "type": "request", "id": "login", "method": "POST", "path": "/login",
      "body": { "username": "{{data.users.username}}" },
      "extract": { "token": "jsonpath:$.token" } },

    { "type": "request", "id": "home", "method": "GET", "path": "/home",
      "headers": { "Authorization": "Bearer {{steps.login.token}}" } }
  ]
}
```

At `home`'s execution, the template `{{steps.login.token}}` is resolved in `home`'s current scope (which is the sequence scope `session`). The sequence scope contains `login.token` because `login` ran earlier in the sequence. Resolution is local — no walk up to the root.

### Example 2: Parallel fan-out with a nested sequence

```json
{
  "type": "sequence",
  "id": "session",
  "children": [
    { "type": "request", "id": "login", "method": "POST", "path": "/login",
      "extract": { "token": "jsonpath:$.token" } },

    { "type": "parallel",
      "id": "fan-out",
      "wait": "all",
      "children": [
        { "type": "sequence", "id": "chain-a", "children": [
          { "type": "request", "id": "follow-a", "method": "POST", "path": "/follow",
            "body": { "token": "{{steps.login.token}}", "target": "alice" },
            "extract": { "follow_id": "jsonpath:$.id" } },
          { "type": "request", "id": "notify-a", "method": "POST", "path": "/notify",
            "body": { "follow_id": "{{steps.follow-a.follow_id}}" } }
        ] },
        { "type": "request", "id": "follow-b", "method": "POST", "path": "/follow",
          "body": { "token": "{{steps.login.token}}", "target": "bob" } }
      ]
    },

    { "type": "request", "id": "summary", "method": "GET",
      "path": "/summary?follow_a={{steps.fan-out.chain-a.follow-a.follow_id}}" }
  ]
}
```

Things this example illustrates:

- `follow-a` and `follow-b` both read `{{steps.login.token}}`. That token is in the outer sequence `session`'s scope, and the resolver walks up from the parallel's child scope into the parallel's parent scope until it finds the match. This walk is legal because `login` completed before the parallel started.
- `notify-a` reads `{{steps.follow-a.follow_id}}`. Both `notify-a` and `follow-a` are inside the same nested sequence `chain-a`, so the read is local to `chain-a`'s scope. This is the only way one parallel branch can see earlier steps of the same branch.
- `follow-b` **cannot** read `{{steps.follow-a.follow_id}}`. The two are siblings inside the same parallel, so they run in independent scopes. If you need a step to depend on a parallel sibling, the dependency must be expressed via a nested sequence (as `chain-a` does for its own children).
- `summary` reads `{{steps.fan-out.chain-a.follow-a.follow_id}}`. After the parallel completes, each child's results are merged into the parallel's parent scope. Their keys are nested under the parallel's `id` (`fan-out`), then under each child's `id` (`chain-a`), then under the innermost request's `id` (`follow-a`). The summary's template walk finds `fan-out.chain-a.follow-a.follow_id` at the `session` scope level.

### Example 3: Optional nested inside a sequence

```json
{
  "type": "sequence",
  "id": "session",
  "children": [
    { "type": "request", "id": "home", "method": "GET", "path": "/home" },

    { "type": "optional",
      "probability": 0.4,
      "child": { "type": "request", "id": "like-post", "method": "POST", "path": "/like",
                 "extract": { "like_id": "jsonpath:$.id" } } },

    { "type": "request", "id": "summary", "method": "GET",
      "path": "/summary?like={{steps.like-post.like_id}}" }
  ]
}
```

- When the optional gate fires, `like-post` runs and merges its `like_id` extract into the session scope under `like-post`. `summary` resolves the template successfully.
- When the gate does not fire, there is no `like-post` entry in the session scope. `summary`'s template `{{steps.like-post.like_id}}` resolves to `undefined`. The engine emits an `atropos.workflow.template.unresolved` span event with attributes `template=steps.like-post.like_id` and `reason=optional_skipped`, then substitutes an empty string into the final URL. This is a deliberate design choice: an optional-dependent template should either be inside the same optional wrapper (so it only runs when the value exists) or should guard against emptiness in the target service. A workflow author who wants "if like-post ran, include it; otherwise skip the summary" should wrap the `summary` step in the same `optional` or nest it inside a sequence inside the optional.

## Data schema handshake

A workflow declares the data it needs via `data_schema.pools`. Manteion owns the dataset and must satisfy that schema before a run starts. Zeus performs the validation handshake in two places:

1. **Pre-run validate.** `POST /api/v1/workflows/{id}/validate?dataset=<id>` returns `{ok: true|false, missing: [...], warnings: [...]}`. Manteion and CLI authors can call this before starting a run to avoid wasted k6 startup time.
2. **Run-start validate.** `POST /api/v1/workflows/{id}/runs` always runs the validator internally. If validation fails, the run never enters `running` — it goes straight to `rejected` with a reason string, and no k6 job is launched.

For each declared pool, the validator checks:

- **Existence** — is there a pool with that name in the dataset?
- **Fields** — does the pool have every listed field? A missing field is a hard error.
- **Min size** — `len(pool) >= min_size`. A missing minimum is a hard error unless `"optional": true`.
- **Type hints** — if the workflow declares `"fields": {"id": "string", "price": "number"}` (the typed form), the validator samples a few rows and verifies.

Validation failure reasons are surfaced back to manteion via the run's `rejected` state, which is terminal. Manteion knows to retry with a different dataset on `rejected` but not on `failed` (which is a runtime failure after execution started).

## Template expression reference

Template expressions in v2 are the same core set as v1 with two additions: `jsonpath:` in `extract` and dot-keyed writes via `data.*` in `variants.set`.

| Expression | Resolved to |
|---|---|
| `{{data.<pool>.<field>}}` | Random row from the data pool (one draw per iteration, cached via pool binding), then field access. Example: `{{data.users.email}}`. |
| `{{data.<pool>.<field>.<index>}}` | Array-indexed access on a field that is itself an array. Example: `{{data.users.follow_ids.0}}` picks the first entry of the current iteration's user's `follow_ids` array. |
| `{{steps.<id>.<key>}}` | Extract from an earlier step. Resolved via the scope tree. Example: `{{steps.login.token}}`. |
| `{{env.<VAR>}}` | k6 environment variable, evaluated at init. Example: `{{env.BASE_URL}}`. |
| `{{random_int(min, max)}}` | Random integer in `[min, max]` inclusive. |
| `{{random_choice(a, b, c, ...)}}` | Random pick from the literal list. |
| `jsonpath:<expr>` | Only valid in `extract` values. Evaluates a JSONPath expression against the response body. Example: `"post_id": "jsonpath:$.post_id"`. |

Unresolved templates emit `atropos.workflow.template.unresolved` span events and substitute an empty string. They do not throw. Authors can detect them via the events in the run's trace stream.

## Migration: online-boutique `browse.json` in v2

The v1 flow in full:

```json
{
  "name": "online-boutique-browse",
  "description": "Browse products and optionally add to cart",
  "targets": ["frontend", "productcatalogservice", "currencyservice", "cartservice"],
  "estimatedRpsPerVu": 2,
  "steps": [
    {
      "name": "homepage",
      "method": "GET",
      "path": "/",
      "expect": [200]
    },
    {
      "name": "view_product",
      "method": "GET",
      "path": "/product/{{pool.products.id}}",
      "requires": "homepage",
      "probability": "explore",
      "expect": [200],
      "extract": { "product_id": "{{pool.products.id}}" }
    },
    {
      "name": "add_to_cart",
      "method": "POST",
      "path": "/cart",
      "requires": "view_product",
      "probability": "engage",
      "body": {
        "product_id": "{{steps.view_product.product_id}}",
        "quantity":   "{{random_int(1,3)}}"
      },
      "expect": [200, 302]
    },
    {
      "name": "view_cart",
      "method": "GET",
      "path": "/cart",
      "requires": "add_to_cart",
      "probability": 0.5,
      "expect": [200]
    }
  ]
}
```

The same flow in v2:

```json
{
  "$schema": "https://faults-lab.dev/zeus/workflow.schema.json",
  "version": "2",
  "name": "online-boutique-browse",
  "description": "Browse products and optionally add to cart",
  "targets": ["frontend", "productcatalogservice", "currencyservice", "cartservice"],
  "base_url": "http://frontend:8080",
  "estimated_rps_per_vu": 2,
  "default_delay": { "min_ms": 3000, "max_ms": 8000 },
  "thresholds": {
    "http_req_failed":   ["rate<0.1"],
    "http_req_duration": ["p(95)<2000"]
  },
  "data_schema": {
    "pools": {
      "products": { "fields": ["id", "name"], "min_size": 5 }
    }
  },
  "root": {
    "type": "sequence",
    "id": "browse",
    "children": [
      {
        "type": "request",
        "id": "homepage",
        "method": "GET",
        "path": "/",
        "expect": { "status": [200] }
      },
      {
        "type": "optional",
        "probability": "explore",
        "child": {
          "type": "sequence",
          "id": "after-homepage",
          "children": [
            {
              "type": "request",
              "id": "view_product",
              "method": "GET",
              "path": "/product/{{data.products.id}}",
              "expect": { "status": [200] },
              "extract": { "product_id": "{{data.products.id}}" }
            },
            {
              "type": "optional",
              "probability": "engage",
              "child": {
                "type": "sequence",
                "id": "cart-flow",
                "children": [
                  {
                    "type": "request",
                    "id": "add_to_cart",
                    "method": "POST",
                    "path": "/cart",
                    "body": {
                      "product_id": "{{steps.view_product.product_id}}",
                      "quantity":   "{{random_int(1,3)}}"
                    },
                    "expect": { "status": [200, 302] }
                  },
                  {
                    "type": "optional",
                    "probability": 0.5,
                    "child": {
                      "type": "request",
                      "id": "view_cart",
                      "method": "GET",
                      "path": "/cart",
                      "expect": { "status": [200] }
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    ]
  }
}
```

The cascade of `optional` wrappers is structurally heavier than the old flat list, but it makes the semantics unambiguous. If a user does not explore, they do not engage. If they do not engage, they do not view the cart. The old flow depended on `requires` chains to enforce the same invariant implicitly and relied on the engine short-circuiting skipped-prerequisite children. In v2 the nesting is explicit, and a tool that generates workflow variants mechanically (manteion) can reason about the structure without interpreting a separate probability table.

A few pointwise notes on the migration:

- **`pool.*` → `data.*`.** v1 used `pool` as the templating prefix for the data bindings; v2 standardizes on `data` so the name matches the schema field.
- **`estimatedRpsPerVu` → `estimated_rps_per_vu`.** Snake case throughout.
- **`expect: [200]` → `expect: {"status": [200]}`.** Object form supports future additions.
- **`"probability": "explore"` on a step → `optional` wrapper around a sequence containing that step.** Opens space for the wrapped sequence to contain dependent structure.
- **`requires` is gone.** Its work is done by tree structure (children of a sequence run in order).
- **`default_delay` replaces implicit persona thinkTime.** Persona thinkTime becomes the numeric value plugged into `default_delay` at run start, or can be referenced by name from individual `delay` nodes via `persona_key`.

## Out of scope

These are deliberate non-goals for the v2 spec so the schema can grow cleanly:

- **Retry and circuit breakers.** A failing request fails. No per-step retry count, no exponential backoff, no circuit-open logic. Workloads that need retry semantics should target the service's own retry surface.
- **Body-shape assertions.** `expect` only checks status codes. No regex match, no JSONPath match, no header assertion. The `expect` object form leaves room for this later.
- **Conditional branching on response content.** The only conditional in v2 is `optional`'s probability gate. Conditional-on-status or conditional-on-body is future work.
- **Streaming responses and SSE consumption.** All requests are simple request/response. Streaming endpoints are out of scope.
- **gRPC.** Everything is HTTP. gRPC workflows can be added by extending the `request` node with a discriminator, but are not in this version.
- **Nested variants.** A request has one `variants` list at one level. No nested variant trees.
- **Dynamic weight computation.** Variant weights are literal numbers, not expressions. Dynamic mixes (e.g., "weight = inverse of current cart size") are not expressible in v2.
