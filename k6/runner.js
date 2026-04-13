/**
 * runner.js - Generic config-driven k6 flow runner (DSL v2).
 *
 * Loads a v2 flow JSON, persona, and data pool, then delegates all
 * execution to the tree-walking engine.
 *
 * Usage:
 *   k6 run -e BASE_URL=http://frontend:8080 \
 *          -e FLOW=online-boutique/browse \
 *          -e PERSONA=cautious \
 *          runner.js
 *
 * Environment variables:
 *   FLOW                    Path relative to flows/ (e.g., "online-boutique/browse")
 *   PERSONA                 Persona filename without .json (default: "cautious")
 *   DATA                    Data pool path override (default: flows/<app>/data.json)
 *   BASE_URL                Target service base URL
 *   VUS                     Virtual users (default: 10)
 *   DURATION                Test duration (default: "5m")
 *   ZEUS_URL                Zeus service URL (default: "http://localhost:8080")
 *   ZEUS_DATASET_ENDPOINT   HTTP endpoint to fetch dataset (skips file-based data)
 *   ZEUS_WORKFLOW_LABEL     Workflow label for atropos.workflow baggage
 *   ZEUS_RUN_ID             Run ID for metrics
 */

import { createEngine } from "./scripts/lib/engine.js";

// ── Derive config file paths from environment variables ───────────

const FLOW = __ENV.FLOW || "online-boutique/browse";
const PERSONA = __ENV.PERSONA || "cautious";

const flowPath = `./flows/${FLOW}.json`;
const personaPath = `./personas/${PERSONA}.json`;

// Data pool: skip file load if dataset comes from zeus HTTP endpoint.
const flowDir = FLOW.substring(0, FLOW.lastIndexOf("/"));
const dataFile = __ENV.DATA || `${flowDir}/data`;
const dataPath = __ENV.ZEUS_DATASET_ENDPOINT ? null : `./flows/${dataFile}.json`;

// ── Create engine in init context (open() works here) ─────────────

const engine = createEngine(flowPath, personaPath, dataPath);

// ── Export k6 lifecycle hooks ─────────────────────────────────────

export const options = engine.options;

export function setup() {
  return engine.setup();
}

export default function (data) {
  engine.run(data);
}

export function teardown(data) {
  engine.teardown(data);
}
