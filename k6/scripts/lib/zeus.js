/**
 * zeus.js - Zeus service client for k6.
 *
 * In DSL v2, zeus owns the run lifecycle; k6 does not self-register.
 * This module provides:
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
 *
 * @param {Function} fn - Function to retry; returns non-null on success
 * @param {string} label - Log label for diagnostics
 * @returns {*} The return value of fn, or throws after exhausting retries
 */
function withRetry(fn, label) {
  for (let attempt = 1; attempt <= MAX_RETRIES; attempt++) {
    try {
      const result = fn();
      if (result !== undefined && result !== null) return result;
    } catch (_e) {
      // fall through to retry
    }
    if (attempt < MAX_RETRIES) {
      console.warn(
        `${label}: attempt ${attempt}/${MAX_RETRIES} failed, retrying in ${RETRY_INTERVAL_S}s...`
      );
      sleep(RETRY_INTERVAL_S);
    }
  }
  throw new Error(`${label}: all ${MAX_RETRIES} attempts exhausted`);
}

/**
 * Wait for zeus to be healthy. Used by init containers.
 */
export function waitForZeus() {
  return withRetry(() => {
    const res = http.get(`${ZEUS_URL}/healthz`);
    return res.status === 200 ? true : undefined;
  }, "waitForZeus");
}

/**
 * Fetch a dataset from zeus's dataset store.
 *
 * @param {string} endpoint - Full URL to the dataset endpoint
 * @returns {Object} Parsed dataset JSON
 * @throws {Error} After MAX_RETRIES failures
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
 * @returns {Object} The attack object with id, status, started_at
 * @throws {Error} If zeus rejects the attack (non-202)
 */
export function triggerAttack(config) {
  const res = http.post(
    `${ZEUS_URL}/api/v1/attacks`,
    JSON.stringify(config),
    { headers: { "Content-Type": "application/json" } }
  );
  const accepted = check(res, {
    "attack accepted (202)": (r) => r.status === 202,
  });
  if (!accepted) {
    throw new Error(`triggerAttack failed: HTTP ${res.status} — ${res.body}`);
  }
  return JSON.parse(res.body);
}

/**
 * Check the status of a running attack.
 *
 * @param {string} attackID - Attack ID
 * @returns {Object} Attack status and results
 * @throws {Error} If the attack is not found (non-200)
 */
export function getAttackStatus(attackID) {
  const res = http.get(`${ZEUS_URL}/api/v1/attacks/${attackID}`);
  if (res.status !== 200) {
    throw new Error(`getAttackStatus(${attackID}): HTTP ${res.status}`);
  }
  return JSON.parse(res.body);
}
