/**
 * engine.js - DSL v2 tree-walking execution engine for k6.
 *
 * Replaces the v1 topoSort + flat step list with a recursive tree walker.
 * Node types: sequence, parallel, delay, optional, request.
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
import { resolveObject } from "./template.js";

// ── Scope chain ─────────────────────────────────────────────────────
//
// Scopes nest following the tree structure. Each scope holds extract
// entries from nodes that ran within it. Resolution walks up the chain:
// if the root key of a path exists at the current level, traverse into
// it; otherwise try the parent scope.

function createScope(parent) {
  return {
    parent: parent,
    entries: {},

    set: function (id, value) {
      this.entries[id] = value;
    },

    merge: function (entries) {
      for (var k of Object.keys(entries)) {
        this.entries[k] = entries[k];
      }
    },

    resolve: function (path) {
      var parts = path.split(".");
      var root = parts[0];
      if (root in this.entries) {
        var current = this.entries[root];
        for (var i = 1; i < parts.length; i++) {
          if (current === null || current === undefined) return undefined;
          var idx = parseInt(parts[i]);
          current = isNaN(idx) ? current[parts[i]] : current[idx];
        }
        return current;
      }
      if (this.parent) return this.parent.resolve(path);
      return undefined;
    },
  };
}

// ── Variant sampling ────────────────────────────────────────────────

function pickVariant(variants) {
  if (!variants || variants.length === 0) return null;
  var total = variants.reduce(function (s, v) {
    return s + (v.weight || 0);
  }, 0);
  if (total === 0) return null;
  var r = Math.random() * total;
  for (var i = 0; i < variants.length; i++) {
    r -= variants[i].weight || 0;
    if (r <= 0) return { variant: variants[i], index: i };
  }
  return { variant: variants[variants.length - 1], index: variants.length - 1 };
}

/**
 * Set a value at a dot-path inside an object, creating intermediates.
 */
function applyPatch(obj, dotPath, value) {
  var parts = dotPath.split(".");
  var current = obj;
  for (var i = 0; i < parts.length - 1; i++) {
    if (current[parts[i]] === undefined || current[parts[i]] === null) {
      current[parts[i]] = {};
    }
    current = current[parts[i]];
  }
  current[parts[parts.length - 1]] = value;
}

// ── Delay ───────────────────────────────────────────────────────────

function sleepDelay(spec, persona) {
  if (!spec) return;
  if (spec.persona_key && persona && persona.delays) {
    var resolved = persona.delays[spec.persona_key];
    if (resolved) spec = resolved;
  }
  if (typeof spec.min_ms === "number" && typeof spec.max_ms === "number") {
    var ms = spec.min_ms + Math.random() * (spec.max_ms - spec.min_ms);
    sleep(ms / 1000);
  }
}

// ── JSON path accessor ──────────────────────────────────────────────

function getJsonPath(obj, path) {
  if (path.startsWith("$.")) path = path.substring(2);
  var parts = path.split(".");
  var current = obj;
  for (var i = 0; i < parts.length; i++) {
    if (current === null || current === undefined) return undefined;
    var idx = parseInt(parts[i]);
    current = isNaN(idx) ? current[parts[i]] : current[idx];
  }
  return current;
}

// ── Probability gate ────────────────────────────────────────────────

function shouldFire(probability, persona) {
  if (probability === undefined || probability === null) return true;
  var prob;
  if (typeof probability === "number") {
    prob = probability;
  } else if (typeof probability === "string") {
    prob =
      persona && persona.probabilities
        ? persona.probabilities[probability]
        : undefined;
    if (prob === undefined) prob = 1.0;
  } else {
    prob = 1.0;
  }
  return Math.random() < prob;
}

// ── Request preparation and response processing ─────────────────────

function prepareRequest(node, scope, ctx) {
  var body = node.body ? JSON.parse(JSON.stringify(node.body)) : null;
  var headers = node.headers ? Object.assign({}, node.headers) : {};
  var path = node.path;
  var method = (node.method || "GET").toUpperCase();
  var dataOverrides = null;

  // Variant selection
  var variantIndex = -1;
  if (node.variants && node.variants.length > 0) {
    var pick = pickVariant(node.variants);
    if (pick) {
      variantIndex = pick.index;
      var working = { body: body, headers: headers, path: path, method: method };
      var setMap = pick.variant.set || {};
      for (var key of Object.keys(setMap)) {
        if (key.startsWith("data.")) {
          if (!dataOverrides) dataOverrides = {};
          applyPatch(dataOverrides, key.substring(5), setMap[key]);
        } else {
          applyPatch(working, key, setMap[key]);
        }
      }
      body = working.body;
      headers = working.headers;
      path = working.path;
      method = working.method;
    }
  }

  // Resolve templates
  var templateCtx = {
    data: ctx.data,
    env: __ENV,
    _resolveSteps: function (p) {
      return scope.resolve(p);
    },
    _dataOverrides: dataOverrides,
  };
  var bundle = {
    path: path,
    headers: headers,
    body: body,
    _extract: node.extract || {},
  };
  var resolved = resolveObject(bundle, templateCtx);

  var url = ctx.baseURL + resolved.path;

  var params = withTracing(
    { headers: resolved.headers, tags: { name: node.id || "unnamed" } },
    ctx.traceID,
    ctx.workflowLabel
  );

  if (node.timeout_ms) {
    params.timeout = String(node.timeout_ms) + "ms";
  }

  // Encode body based on Content-Type
  var encodedBody = null;
  if (resolved.body !== null && resolved.body !== undefined) {
    var ct = (resolved.headers["Content-Type"] || "").toLowerCase();
    encodedBody = ct.includes("application/json")
      ? JSON.stringify(resolved.body)
      : resolved.body;
  }

  return {
    method: method,
    url: url,
    body: encodedBody,
    params: params,
    resolvedExtracts: resolved._extract,
    variantIndex: variantIndex,
    batchEntry: [method, url, encodedBody, params],
  };
}

function processResponse(node, response, resolvedExtracts, scope) {
  var passed = true;

  if (node.expect && node.expect.status) {
    var label =
      (node.id || "unnamed") +
      " status in [" +
      node.expect.status.join(",") +
      "]";
    passed = check(response, {
      [label]: function (r) {
        return node.expect.status.includes(r.status);
      },
    });
  }

  if (node.extract && node.id) {
    var extracted = {};
    for (var [key, expr] of Object.entries(node.extract)) {
      if (typeof expr === "string" && expr.startsWith("jsonpath:")) {
        try {
          var respBody = JSON.parse(response.body);
          extracted[key] = getJsonPath(respBody, expr.substring(9));
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

function walkNode(node, scope, ctx) {
  switch (node.type) {
    case "sequence":
      return walkSequence(node, scope, ctx);
    case "parallel":
      return walkParallel(node, scope, ctx);
    case "delay":
      return walkDelay(node, scope, ctx);
    case "optional":
      return walkOptional(node, scope, ctx);
    case "request":
      return walkRequest(node, scope, ctx);
    default:
      throw new Error('engine: unknown node type "' + node.type + '"');
  }
}

/**
 * Sequence: children run in order. Later children see earlier siblings'
 * extracts. Short-circuits on child failure.
 */
function walkSequence(node, scope, ctx) {
  var seqScope = createScope(scope);
  var ok = true;

  for (var i = 0; i < node.children.length; i++) {
    ok = walkNode(node.children[i], seqScope, ctx);
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
  var children = node.children || [];
  if (children.length === 0) return true;

  var allRequests = children.every(function (c) {
    return c.type === "request";
  });

  if (allRequests && children.length > 1) {
    return walkParallelBatch(node, scope, ctx);
  }
  return walkParallelSequential(node, scope, ctx);
}

function walkParallelBatch(node, scope, ctx) {
  var children = node.children;
  var preparations = [];

  for (var i = 0; i < children.length; i++) {
    var childScope = createScope(scope);
    var prep = prepareRequest(children[i], childScope, ctx);
    preparations.push({ prep: prep, scope: childScope, node: children[i] });
  }

  var batchEntries = preparations.map(function (p) {
    return p.prep.batchEntry;
  });
  var responses = http.batch(batchEntries);

  var passCount = 0;
  for (var j = 0; j < children.length; j++) {
    var ok = processResponse(
      preparations[j].node,
      responses[j],
      preparations[j].prep.resolvedExtracts,
      preparations[j].scope
    );
    if (ok) passCount++;
  }

  mergeParallelChildren(node, scope, preparations);
  return checkWaitPolicy(node.wait, passCount, children.length);
}

function walkParallelSequential(node, scope, ctx) {
  var children = node.children;
  var childEntries = [];
  var passCount = 0;

  for (var i = 0; i < children.length; i++) {
    var childScope = createScope(scope);
    var ok = walkNode(children[i], childScope, ctx);
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
  var merged = {};
  for (var i = 0; i < childEntries.length; i++) {
    var entries = childEntries[i].scope.entries;
    for (var k of Object.keys(entries)) {
      merged[k] = entries[k];
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
    var ms = node.min_ms + Math.random() * (node.max_ms - node.min_ms);
    sleep(ms / 1000);
  }
  return true;
}

/** Optional: probability gate. Scope-transparent — child writes to parent. */
function walkOptional(node, scope, ctx) {
  if (!shouldFire(node.probability, ctx.persona)) {
    return true;
  }
  return walkNode(node.child, scope, ctx);
}

/** Request: HTTP call with variant selection, template resolution, extracts. */
function walkRequest(node, scope, ctx) {
  // Before-delay (opt-in only, no default)
  if (node.before_delay !== undefined && node.before_delay !== null) {
    sleepDelay(node.before_delay, ctx.persona);
  }

  var prep = prepareRequest(node, scope, ctx);

  var res;
  switch (prep.method) {
    case "GET":
      res = http.get(prep.url, prep.params);
      break;
    case "POST":
      res = http.post(prep.url, prep.body, prep.params);
      break;
    case "PUT":
      res = http.put(prep.url, prep.body, prep.params);
      break;
    case "PATCH":
      res = http.patch(prep.url, prep.body, prep.params);
      break;
    case "DELETE":
      res = http.del(prep.url, null, prep.params);
      break;
    default:
      res = http.request(prep.method, prep.url, prep.body, prep.params);
  }

  var ok = processResponse(node, res, prep.resolvedExtracts, scope);

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
  var flow = JSON.parse(open(flowPath));
  var persona = JSON.parse(open(personaPath));
  var fileData = dataPath ? JSON.parse(open(dataPath)) : null;

  if (flow.version !== "2" || !flow.root) {
    throw new Error(
      'engine: flow must be DSL v2 format (version: "2" with root node). ' +
        "See docs/workflow-dsl-v2.md for the migration guide."
    );
  }

  var baseURL = __ENV.BASE_URL || flow.base_url || "http://localhost:8080";
  var defaultDelay = flow.default_delay || null;
  var metaTraceID = generateMetaTraceID();
  var workflowLabel = __ENV.ZEUS_WORKFLOW_LABEL || flow.name;

  var engineOptions = {
    vus: __ENV.VUS ? parseInt(__ENV.VUS) : 10,
    duration: __ENV.DURATION || "5m",
    thresholds: flow.thresholds || {
      http_req_failed: ["rate<0.1"],
      http_req_duration: ["p(95)<2000"],
    },
  };

  return {
    flow: flow,
    options: engineOptions,

    setup: function () {
      var data = fileData;
      if (__ENV.ZEUS_DATASET_ENDPOINT) {
        var res = http.get(__ENV.ZEUS_DATASET_ENDPOINT, {
          headers: { Accept: "application/json" },
        });
        if (res.status !== 200) {
          throw new Error(
            "engine: failed to fetch dataset from " +
              __ENV.ZEUS_DATASET_ENDPOINT +
              ": HTTP " +
              res.status
          );
        }
        data = JSON.parse(res.body);
      }
      if (!data) {
        throw new Error(
          "engine: no dataset available. Set DATA env var or ZEUS_DATASET_ENDPOINT."
        );
      }
      return {
        data: data,
        metaTraceID: metaTraceID,
        workflowLabel: workflowLabel,
      };
    },

    run: function (setupData) {
      var rootScope = createScope(null);
      var ctx = {
        flow: flow,
        persona: persona,
        data: setupData.data,
        baseURL: baseURL,
        traceID: setupData.metaTraceID,
        workflowLabel: setupData.workflowLabel,
        defaultDelay: defaultDelay,
      };
      walkNode(flow.root, rootScope, ctx);
    },

    teardown: function (_setupData) {
      // No-op: zeus owns the run lifecycle; k6 does not self-register.
    },
  };
}
