/**
 * engine.js - DSL v2 tree-walking execution engine for k6.
 *
 * Replaces the v1 topoSort + flat step list with a recursive tree walker.
 * Node types: sequence, parallel, delay, optional, request, repeat, if.
 *
 * Execution model per VU iteration:
 *   1. Walk the root node tree recursively
 *   2. Scoped extract resolution (walk up scope chain for {{steps.*}})
 *   3. Variant sampling (weighted, sparse dot-path patches)
 *   4. Template resolution: {{data.*}}, {{steps.*}}, {{random_*}}, {{env.*}}
 *   5. HTTP request with W3C baggage tracing (meta-trace-id + atropos.workflow)
 *   6. Response check + JSONPath extraction
 *   7. Per-node delay handling (default_delay, before_delay, after_delay)
 */

import http from "k6/http";
import { check, sleep } from "k6";
import { generateMetaTraceID, withTracing } from "./tracing.js";
import { resolveObject, evalTruthy, evalInt } from "./template.js";

// ── Constants ───────────────────────────────────────────────────────

const NODE_SEQUENCE = "sequence";
const NODE_PARALLEL = "parallel";
const NODE_DELAY = "delay";
const NODE_OPTIONAL = "optional";
const NODE_REQUEST = "request";
const NODE_REPEAT = "repeat";
const NODE_IF = "if";

/** Default safety cap for `repeat` when only `while` is set without `max`. */
const REPEAT_DEFAULT_MAX = 100;

const JSONPATH_PREFIX = "jsonpath:";

/** HTTP method dispatch: maps method string to k6 http function. */
const HTTP_METHODS = {
  GET: (url, _body, params) => http.get(url, params),
  POST: (url, body, params) => http.post(url, body, params),
  PUT: (url, body, params) => http.put(url, body, params),
  PATCH: (url, body, params) => http.patch(url, body, params),
  DELETE: (url, _body, params) => http.del(url, null, params),
};

// ── Scope chain ─────────────────────────────────────────────────────
//
// Scopes nest following the tree structure. Each scope holds extract
// entries from nodes that ran within it. Resolution walks up the chain:
// if the root key of a path exists at the current level, traverse into
// it; otherwise try the parent scope.

function createScope(parent) {
  return {
    parent,
    entries: {},

    set(id, value) {
      this.entries[id] = value;
    },

    merge(incoming) {
      for (const [k, v] of Object.entries(incoming)) {
        this.entries[k] = v;
      }
    },

    resolve(path) {
      const parts = path.split(".");
      const root = parts[0];
      if (root in this.entries) {
        let current = this.entries[root];
        for (let i = 1; i < parts.length; i++) {
          if (current == null) return undefined;
          const idx = parseInt(parts[i]);
          current = isNaN(idx) ? current[parts[i]] : current[idx];
        }
        return current;
      }
      return this.parent ? this.parent.resolve(path) : undefined;
    },
  };
}

// ── Variant sampling ────────────────────────────────────────────────

function pickVariant(variants) {
  if (!variants || variants.length === 0) return null;
  const total = variants.reduce((sum, v) => sum + (v.weight || 0), 0);
  if (total === 0) return null;
  let r = Math.random() * total;
  for (let i = 0; i < variants.length; i++) {
    r -= variants[i].weight || 0;
    if (r <= 0) return { variant: variants[i], index: i };
  }
  return { variant: variants[variants.length - 1], index: variants.length - 1 };
}

/**
 * Set a value at a dot-path inside an object, creating intermediates.
 */
function applyPatch(obj, dotPath, value) {
  const parts = dotPath.split(".");
  let current = obj;
  for (let i = 0; i < parts.length - 1; i++) {
    if (current[parts[i]] == null) {
      current[parts[i]] = {};
    }
    current = current[parts[i]];
  }
  current[parts[parts.length - 1]] = value;
}

// ── Delay ───────────────────────────────────────────────────────────

function sleepDelay(spec, persona) {
  if (!spec) return;
  if (spec.persona_key && persona?.delays) {
    const resolved = persona.delays[spec.persona_key];
    if (resolved) spec = resolved;
  }
  if (typeof spec.min_ms === "number" && typeof spec.max_ms === "number") {
    const ms = spec.min_ms + Math.random() * (spec.max_ms - spec.min_ms);
    sleep(ms / 1000);
  }
}

// ── JSON path accessor ──────────────────────────────────────────────

function getJsonPath(obj, path) {
  let p = path.startsWith("$.") ? path.substring(2) : path;
  let current = obj;
  for (const part of p.split(".")) {
    if (current == null) return undefined;
    const idx = parseInt(part);
    current = isNaN(idx) ? current[part] : current[idx];
  }
  return current;
}

// ── Probability gate ────────────────────────────────────────────────

function shouldFire(probability, persona) {
  if (probability == null) return true;
  let prob;
  if (typeof probability === "number") {
    prob = probability;
  } else if (typeof probability === "string") {
    prob = persona?.probabilities?.[probability] ?? 1.0;
  } else {
    prob = 1.0;
  }
  return Math.random() < prob;
}

// ── Request preparation and response processing ─────────────────────

function prepareRequest(node, scope, ctx) {
  let body = node.body ? JSON.parse(JSON.stringify(node.body)) : null;
  let headers = node.headers ? { ...node.headers } : {};
  let path = node.path;
  let method = (node.method || "GET").toUpperCase();
  let dataOverrides = null;

  // Variant selection
  let variantIndex = -1;
  if (node.variants?.length > 0) {
    const pick = pickVariant(node.variants);
    if (pick) {
      variantIndex = pick.index;
      const working = { body, headers, path, method };
      const setMap = pick.variant.set || {};
      for (const [key, value] of Object.entries(setMap)) {
        if (key.startsWith("data.")) {
          if (!dataOverrides) dataOverrides = {};
          applyPatch(dataOverrides, key.substring(5), value);
        } else {
          applyPatch(working, key, value);
        }
      }
      ({ body, headers, path, method } = working);
    }
  }

  // Resolve templates
  const templateCtx = {
    data: ctx.data,
    env: __ENV,
    _resolveSteps: (p) => scope.resolve(p),
    _dataOverrides: dataOverrides,
  };
  const resolved = resolveObject(
    { path, headers, body, _extract: node.extract || {} },
    templateCtx,
  );

  const url = ctx.baseURL + resolved.path;

  const params = withTracing(
    { headers: resolved.headers, tags: { name: node.id || "unnamed" } },
    ctx.traceID,
    ctx.workflowLabel,
  );

  if (node.timeout_ms) {
    params.timeout = `${node.timeout_ms}ms`;
  }

  // Encode body based on Content-Type
  let encodedBody = null;
  if (resolved.body != null) {
    const ct = (resolved.headers["Content-Type"] || "").toLowerCase();
    encodedBody = ct.includes("application/json")
      ? JSON.stringify(resolved.body)
      : resolved.body;
  }

  return {
    method,
    url,
    body: encodedBody,
    params,
    resolvedExtracts: resolved._extract,
    variantIndex,
    batchEntry: [method, url, encodedBody, params],
  };
}

function processResponse(node, response, resolvedExtracts, scope) {
  let passed = true;

  if (node.expect?.status) {
    const label = `${node.id || "unnamed"} status in [${node.expect.status.join(",")}]`;
    passed = check(response, {
      [label]: (r) => node.expect.status.includes(r.status),
    });
  }

  if (node.extract && node.id) {
    const extracted = {};
    for (const [key, expr] of Object.entries(node.extract)) {
      if (typeof expr === "string" && expr.startsWith(JSONPATH_PREFIX)) {
        try {
          const respBody = JSON.parse(response.body);
          extracted[key] = getJsonPath(respBody, expr.substring(JSONPATH_PREFIX.length));
        } catch (_e) {
          extracted[key] = undefined;
        }
      } else {
        extracted[key] = resolvedExtracts[key];
      }
    }
    scope.set(node.id, extracted);
  }

  return passed;
}

// ── Node walkers ────────────────────────────────────────────────────

/** Dispatch table for node type → walker function. */
const NODE_WALKERS = {
  [NODE_SEQUENCE]: walkSequence,
  [NODE_PARALLEL]: walkParallel,
  [NODE_DELAY]: walkDelay,
  [NODE_OPTIONAL]: walkOptional,
  [NODE_REQUEST]: walkRequest,
  [NODE_REPEAT]: walkRepeat,
  [NODE_IF]: walkIf,
};

/**
 * Build a template-resolution context that reads steps from the given scope.
 * Shared by walkRepeat and walkIf for condition/count evaluation.
 */
function templateContextFor(scope, ctx) {
  return {
    data: ctx.data,
    env: __ENV,
    _resolveSteps: (p) => scope.resolve(p),
  };
}

function walkNode(node, scope, ctx) {
  const walker = NODE_WALKERS[node.type];
  if (!walker) throw new Error(`engine: unknown node type "${node.type}"`);
  return walker(node, scope, ctx);
}

/**
 * Sequence: children run in order. Later children see earlier siblings'
 * extracts. Short-circuits on child failure.
 */
function walkSequence(node, scope, ctx) {
  const seqScope = createScope(scope);
  let ok = true;

  for (const child of node.children) {
    ok = walkNode(child, seqScope, ctx);
    if (!ok) break;
  }

  if (node.id) {
    scope.set(node.id, seqScope.entries);
  } else {
    scope.merge(seqScope.entries);
  }
  return ok;
}

/**
 * Parallel: children run in independent scopes. Uses http.batch when all
 * children are simple request nodes; falls back to sequential execution
 * with scope isolation for composite children.
 */
function walkParallel(node, scope, ctx) {
  const children = node.children || [];
  if (children.length === 0) return true;

  const allRequests = children.every((c) => c.type === NODE_REQUEST);

  return allRequests && children.length > 1
    ? walkParallelBatch(node, scope, ctx)
    : walkParallelSequential(node, scope, ctx);
}

function walkParallelBatch(node, scope, ctx) {
  const { children } = node;
  const preparations = children.map((child) => {
    const childScope = createScope(scope);
    const prep = prepareRequest(child, childScope, ctx);
    return { prep, scope: childScope, node: child };
  });

  const batchEntries = preparations.map((p) => p.prep.batchEntry);
  const responses = http.batch(batchEntries);

  let passCount = 0;
  for (let j = 0; j < children.length; j++) {
    const { node: childNode, prep, scope: childScope } = preparations[j];
    const ok = processResponse(childNode, responses[j], prep.resolvedExtracts, childScope);
    if (ok) passCount++;
  }

  mergeParallelChildren(node, scope, preparations);
  return checkWaitPolicy(node.wait, passCount, children.length);
}

function walkParallelSequential(node, scope, ctx) {
  const { children } = node;
  const childEntries = [];
  let passCount = 0;

  for (const child of children) {
    const childScope = createScope(scope);
    const ok = walkNode(child, childScope, ctx);
    if (ok) passCount++;
    childEntries.push({ scope: childScope });
  }

  mergeParallelChildren(node, scope, childEntries);
  return checkWaitPolicy(node.wait, passCount, children.length);
}

/** Check if enough children passed per the wait policy. */
function checkWaitPolicy(wait, passCount, totalCount) {
  if (!wait || wait === "all") return passCount === totalCount;
  if (wait === "any") return passCount > 0;
  if (typeof wait === "object" && wait.n_of_m) return passCount >= wait.n_of_m;
  return passCount === totalCount;
}

function mergeParallelChildren(node, scope, childEntries) {
  const merged = {};
  for (const entry of childEntries) {
    for (const [k, v] of Object.entries(entry.scope.entries)) {
      merged[k] = v;
    }
  }

  if (node.id) {
    scope.set(node.id, merged);
  } else {
    scope.merge(merged);
  }
}

/** Delay: explicit sleep node. */
function walkDelay(node, _scope, ctx) {
  if (node.persona_key) {
    sleepDelay({ persona_key: node.persona_key }, ctx.persona);
  } else if (typeof node.min_ms === "number" && typeof node.max_ms === "number") {
    const ms = node.min_ms + Math.random() * (node.max_ms - node.min_ms);
    sleep(ms / 1000);
  }
  return true;
}

/** Optional: probability gate. Scope-transparent — child writes to parent. */
function walkOptional(node, scope, ctx) {
  return shouldFire(node.probability, ctx.persona)
    ? walkNode(node.child, scope, ctx)
    : true;
}

/**
 * Repeat: run `child` multiple times. Iteration modes (checked in priority
 * order):
 *   - count: literal integer N
 *   - count_template: template expression resolved once to an integer
 *   - while: template expression re-evaluated before each iteration; max
 *            caps the iteration count (default REPEAT_DEFAULT_MAX)
 *
 * All iterations share one loopScope chained to the parent, so iteration
 * K+1 observes iteration K's extracts via {{steps.*}}. Later-iteration
 * writes to the same extract id overwrite earlier ones. loopScope merges
 * back to the parent when the loop ends (or is bound to node.id).
 *
 * Short-circuits on child failure (a failed iteration ends the loop).
 */
function walkRepeat(node, scope, ctx) {
  const loopScope = createScope(scope);
  let iterations;
  let useWhile = false;

  if (typeof node.count === "number") {
    iterations = node.count;
  } else if (typeof node.count_template === "string") {
    iterations = evalInt(node.count_template, templateContextFor(scope, ctx));
  } else if (typeof node.while === "string") {
    useWhile = true;
    iterations = typeof node.max === "number" ? node.max : REPEAT_DEFAULT_MAX;
  } else {
    throw new Error(
      "engine: repeat node requires count, count_template, or while",
    );
  }

  let ok = true;
  for (let i = 0; i < iterations; i++) {
    if (useWhile && !evalTruthy(node.while, templateContextFor(loopScope, ctx))) {
      break;
    }
    ok = walkNode(node.child, loopScope, ctx);
    if (!ok) break;
  }

  if (node.id) {
    scope.set(node.id, loopScope.entries);
  } else {
    scope.merge(loopScope.entries);
  }
  return ok;
}

/**
 * If: evaluate `condition` as a template expression, take the `then` branch
 * when truthy and `else` branch otherwise. Scope-transparent: whichever
 * branch runs writes to the parent scope. Returns true (success) when the
 * condition is falsy and no `else` is provided — a no-op, not a failure.
 */
function walkIf(node, scope, ctx) {
  if (evalTruthy(node.condition, templateContextFor(scope, ctx))) {
    return walkNode(node.then, scope, ctx);
  }
  if (node.else) {
    return walkNode(node.else, scope, ctx);
  }
  return true;
}

/** Request: HTTP call with variant selection, template resolution, extracts. */
function walkRequest(node, scope, ctx) {
  // Before-delay (opt-in only, no default)
  if (node.before_delay != null) {
    sleepDelay(node.before_delay, ctx.persona);
  }

  const prep = prepareRequest(node, scope, ctx);

  const httpFn = HTTP_METHODS[prep.method];
  const res = httpFn
    ? httpFn(prep.url, prep.body, prep.params)
    : http.request(prep.method, prep.url, prep.body, prep.params);

  const ok = processResponse(node, res, prep.resolvedExtracts, scope);

  // After-delay: explicit override, explicit null (suppress), or default
  if (node.after_delay === null) {
    // Explicitly suppressed — no delay
  } else if (node.after_delay !== undefined) {
    sleepDelay(node.after_delay, ctx.persona);
  } else if (ctx.defaultDelay) {
    sleepDelay(ctx.defaultDelay, ctx.persona);
  }

  return ok;
}

// ── Engine factory ──────────────────────────────────────────────────

/**
 * Create a v2 flow execution engine. Must be called in k6 init context
 * (top-level module scope) because it uses open() to load JSON files.
 *
 * @param {string} flowPath - Path to DSL v2 flow JSON file
 * @param {string} personaPath - Path to persona JSON file
 * @param {string|null} dataPath - Path to data pool JSON (null if using HTTP)
 * @returns {object} Engine with options, setup(), run(), teardown()
 */
export function createEngine(flowPath, personaPath, dataPath) {
  const flow = JSON.parse(open(flowPath));
  const persona = JSON.parse(open(personaPath));
  const fileData = dataPath ? JSON.parse(open(dataPath)) : null;

  if (flow.version !== "2" || !flow.root) {
    throw new Error(
      "engine: flow must be DSL v2 format (version: \"2\" with root node). " +
        "See docs/workflow-dsl-v2.md for the migration guide.",
    );
  }

  const baseURL = __ENV.BASE_URL || flow.base_url || "http://localhost:8080";
  const defaultDelay = flow.default_delay || null;
  const metaTraceID = __ENV.ZEUS_META_TRACE_ID || generateMetaTraceID();
  const workflowLabel = __ENV.ZEUS_WORKFLOW_LABEL || flow.name;

  const vus = __ENV.VUS ? parseInt(__ENV.VUS) : 10;
  const duration = __ENV.DURATION || "5m";
  const rpsPerVU = flow.estimated_rps_per_vu || 1;
  const targetRPS = vus * rpsPerVU;

  // Open-loop generator: hold offered RPS constant across phases so a cached
  // service does not implicitly inflate load on un-frozen services. See the
  // "closed-loop generator" entry under VISION.md "Known confounds".
  const options = {
    scenarios: {
      workflow: {
        executor: "constant-arrival-rate",
        rate: targetRPS,
        timeUnit: "1s",
        duration: duration,
        preAllocatedVUs: vus,
        maxVUs: vus * 4,
      },
    },
    thresholds: {
      ...(flow.thresholds || {
        http_req_failed: ["rate<0.1"],
        http_req_duration: ["p(95)<2000"],
      }),
      dropped_iterations: ["count==0"],
    },
  };

  return {
    flow,
    options,

    setup() {
      let data = fileData;
      if (__ENV.ZEUS_DATASET_ENDPOINT) {
        const res = http.get(__ENV.ZEUS_DATASET_ENDPOINT, {
          headers: { Accept: "application/json" },
        });
        if (res.status !== 200) {
          throw new Error(
            `engine: failed to fetch dataset from ${__ENV.ZEUS_DATASET_ENDPOINT}: HTTP ${res.status}`,
          );
        }
        data = JSON.parse(res.body);
      }
      if (!data) {
        throw new Error(
          "engine: no dataset available. Set DATA env var or ZEUS_DATASET_ENDPOINT.",
        );
      }
      return { data, metaTraceID, workflowLabel };
    },

    run(setupData) {
      const rootScope = createScope(null);
      walkNode(flow.root, rootScope, {
        flow,
        persona,
        data: setupData.data,
        baseURL,
        traceID: setupData.metaTraceID,
        workflowLabel: setupData.workflowLabel,
        defaultDelay,
      });
    },

    teardown(_setupData) {
      // No-op: zeus owns the run lifecycle; k6 does not self-register.
    },
  };
}
