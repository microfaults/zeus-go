import http from "k6/http";
import { check, sleep } from "k6";

// ARCHER_URL is the base URL of the archer Go service.
// Override via environment variable: k6 run -e ARCHER_URL=http://host:port ...
const ARCHER_URL = __ENV.ARCHER_URL || "http://localhost:8080";

const MAX_RETRIES = 10;
const RETRY_INTERVAL_S = 3;

/**
 * Retry wrapper for setup-phase calls that must succeed.
 * Archer may not be ready yet when k6 Jobs start; this avoids a
 * hard crash during the setup() lifecycle hook.
 */
function withRetry(fn, label) {
  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    try {
      const result = fn();
      if (result !== undefined && result !== null) return result;
    } catch (_e) {
      // fall through to retry
    }
    console.warn(
      `${label}: attempt ${attempt}/${MAX_RETRIES} failed, retrying in ${RETRY_INTERVAL_S}s...`
    );
    sleep(RETRY_INTERVAL_S);
  }
  console.error(`${label}: all ${MAX_RETRIES} attempts failed`);
  return null;
}

/**
 * Register an active workload with archer so it knows which services
 * are under load and can target them for precision attacks.
 *
 * @param {Object} workload - Workload metadata
 * @param {string} workload.name - Workload name (e.g., "boutique-browse")
 * @param {string} workload.profile - User persona (e.g., "window-shopper")
 * @param {string[]} workload.targets - Services being hit
 * @param {number} workload.vus - Number of virtual users
 * @param {number} workload.rate - Estimated req/s
 * @param {string} workload.meta_trace_id - Cross-mesh trace correlation ID
 * @param {string} workload.status - Workload status
 * @returns {Object} The registered workload with server-assigned ID
 */
export function registerWorkload(workload) {
  return withRetry(() => {
    const res = http.post(
      `${ARCHER_URL}/api/v1/workloads`,
      JSON.stringify(workload),
      { headers: { "Content-Type": "application/json" } }
    );
    const ok = check(res, {
      "workload registered (201)": (r) => r.status === 201,
    });
    if (!ok) return undefined;
    return JSON.parse(res.body);
  }, "registerWorkload");
}

/**
 * Deregister a workload when the k6 test finishes.
 *
 * @param {string} id - The workload ID returned by registerWorkload
 */
export function deregisterWorkload(id) {
  if (!id) return;
  const res = http.del(`${ARCHER_URL}/api/v1/workloads/${id}`);
  check(res, {
    "workload deregistered (204)": (r) => r.status === 204,
  });
}

/**
 * Trigger a targeted vegeta attack via archer.
 *
 * @param {Object} config - Attack configuration
 * @returns {Object} The attack object with status
 */
export function triggerAttack(config) {
  const res = http.post(
    `${ARCHER_URL}/api/v1/attacks`,
    JSON.stringify(config),
    { headers: { "Content-Type": "application/json" } }
  );
  check(res, {
    "attack accepted (202)": (r) => r.status === 202,
  });
  return JSON.parse(res.body);
}

/**
 * Check the status of a running attack.
 *
 * @param {string} id - Attack ID
 * @returns {Object} Attack status and results
 */
export function getAttackStatus(id) {
  const res = http.get(`${ARCHER_URL}/api/v1/attacks/${id}`);
  return JSON.parse(res.body);
}

/**
 * Fetch the current state of a registered workload.
 * Used by the engine each iteration to check if Manteion has paused
 * or stopped this flow via PATCH /api/v1/workloads/{id}.
 *
 * @param {string} id - Workload ID returned by registerWorkload
 * @returns {Object|null} The workload object (includes status field), or null on error
 */
export function getWorkload(id) {
  const res = http.get(`${ARCHER_URL}/api/v1/workloads/${id}`);
  if (res.status !== 200) return null;
  return JSON.parse(res.body);
}
