/**
 * Trace context helpers for cross-mesh correlation.
 *
 * k6 generates a meta-trace-id that gets injected into W3C Baggage headers
 * on all outgoing requests. This ID:
 * 1. Is registered with zeus as part of run metadata
 * 2. Survives Envoy/Istio proxy propagation via the baggage header
 * 3. Allows correlating traces across mesh boundaries
 *
 * In v2, the baggage also carries atropos.workflow — the workflow label that
 * atropos SDKs read for rule scoping (resolves ambiguity A9).
 */

/**
 * Generate a random hex meta-trace-id (32 hex chars = 16 bytes).
 */
export function generateMetaTraceID() {
  const chars = "0123456789abcdef";
  let id = "";
  for (let i = 0; i < 32; i++) {
    id += chars[Math.floor(Math.random() * 16)];
  }
  return id;
}

/**
 * Inject meta-trace-id and atropos.workflow into a k6 request params
 * object via W3C Baggage header.
 *
 * @param {Object} params - k6 request params (or empty object)
 * @param {string} metaTraceID - The meta-trace-id to inject
 * @param {string} [workflowLabel] - Workflow label for atropos rule scoping
 * @returns {Object} params with baggage header set
 */
export function withTracing(params, metaTraceID, workflowLabel) {
  params = params || {};
  params.headers = params.headers || {};

  const entries = ["meta-trace-id=" + metaTraceID];
  if (workflowLabel) {
    entries.push("atropos.workflow=" + workflowLabel);
  }

  const newBaggage = entries.join(",");
  const existing = params.headers["baggage"] || "";
  params.headers["baggage"] = existing
    ? existing + "," + newBaggage
    : newBaggage;

  return params;
}

/**
 * Create a tagged params object that includes both tracing and k6 tags.
 *
 * @param {string} metaTraceID - The meta-trace-id
 * @param {Object} tags - Additional k6 tags
 * @param {string} [workflowLabel] - Workflow label for atropos
 * @returns {Object} k6 params with baggage header and tags
 */
export function withTracingAndTags(metaTraceID, tags, workflowLabel) {
  return withTracing({ tags: tags || {} }, metaTraceID, workflowLabel);
}
