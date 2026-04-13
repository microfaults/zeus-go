/**
 * zeus.js - Zeus service client for k6.
 *
 * Replaces archer.js. In DSL v2, zeus owns the run lifecycle; k6 does
 * not self-register as a workload. This module provides:
 *   - waitForZeus(): health-check polling for init container readiness
 *   - fetchDataset(): fetch dataset from zeus API
 *   - triggerAttack() / getAttackStatus(): precision attack control
 */

import http from "k6/http";
import { check, sleep } from "k6";

const ZEUS_URL = __ENV.ZEUS_URL || "http://localhost:8080";
const MAX_RETRIES = 10;
const RETRY_INTERVAL_S = 3;

/**
 * Retry wrapper for setup-phase calls that must succeed.
 * Zeus may not be ready yet when k6 Jobs start.
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
 * Wait for zeus to be healthy. Used by init containers.
 */
export function waitForZeus() {
  return withRetry(() => {
    const res = http.get(`${ZEUS_URL}/healthz`);
    if (res.status === 200) return true;
    return undefined;
  }, "waitForZeus");
}

/**
 * Fetch a dataset from zeus's dataset store.
 *
 * @param {string} endpoint - Full URL to the dataset endpoint
 * @returns {Object|null} Parsed dataset JSON, or null on failure
 */
export function fetchDataset(endpoint) {
  return withRetry(() => {
    const res = http.get(endpoint, {
      headers: { Accept: "application/json" },
    });
    if (res.status !== 200) return undefined;
    return JSON.parse(res.body);
  }, "fetchDataset");
}

/**
 * Trigger a targeted vegeta attack via zeus.
 *
 * @param {Object} config - Attack configuration
 * @returns {Object} The attack object with status
 */
export function triggerAttack(config) {
  const res = http.post(
    `${ZEUS_URL}/api/v1/attacks`,
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
  const res = http.get(`${ZEUS_URL}/api/v1/attacks/${id}`);
  return JSON.parse(res.body);
}
