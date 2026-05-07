/**
 * template.js - Lightweight {{expression}} parser and evaluator.
 *
 * Evaluates template expressions against a context containing:
 *   - data: data pools (named collections of objects)
 *   - steps: extracted values from previously executed flow steps
 *   - env: k6 __ENV variables
 *
 * Expression syntax:
 *   {{data.<collection>.<field>}}    - Random item from pool, access field
 *   {{data.<collection>.<field>.<i>}} - Array-indexed access on a field
 *   {{steps.<id>.<key>}}             - Value from a previous step's extract
 *   {{random_int(min,max)}}          - Random integer in [min, max]
 *   {{random_choice(a,b,c)}}         - Random pick from comma-separated literals
 *   {{env.VAR_NAME}}                 - k6 __ENV variable
 *
 * Data cache: within a single resolveObject() call, repeated references to
 * the same data collection return the same random item. This ensures that
 * {{data.products.id}} in a URL and {{data.products.name}} in the body
 * of the same step refer to the same product.
 *
 * Scope-chain resolution: when _resolveSteps is set in the context, steps.*
 * expressions are resolved via the scope chain (DSL v2 tree-walking engine).
 * Falls back to flat lookup if _resolveSteps is not provided.
 */

const EXPR_RE = /\{\{(.+?)\}\}/g;

const PREFIX_DATA = "data.";
const PREFIX_STEPS = "steps.";
const PREFIX_ENV = "env.";
const PREFIX_JSONPATH = "jsonpath:";

/**
 * Evaluate a single template expression against the context.
 */
function evaluateExpression(expr, context) {
  expr = expr.trim();

  // data.<collection>.<field>
  if (expr.startsWith(PREFIX_DATA)) {
    const [, collection, ...rest] = expr.split(".");
    const field = rest.join(".");

    // Check variant overrides first.
    if (context._dataOverrides) {
      const overridePath = collection + (field ? "." + field : "");
      const override = getNestedField(context._dataOverrides, overridePath);
      if (override !== undefined) return override;
    }

    if (!context._dataCache[collection]) {
      const items = context.data?.[collection];
      if (!items || items.length === 0) return undefined;
      context._dataCache[collection] =
        items[Math.floor(Math.random() * items.length)];
    }

    return getNestedField(context._dataCache[collection], field);
  }

  // steps.<id>.<key> — scope-chain resolution in v2, flat lookup fallback
  if (expr.startsWith(PREFIX_STEPS)) {
    const path = expr.substring(PREFIX_STEPS.length);
    if (context._resolveSteps) return context._resolveSteps(path);

    const [stepName, ...keyParts] = path.split(".");
    const key = keyParts.join(".");
    const stepData = context.steps?.[stepName];
    if (!stepData) return undefined;
    return getNestedField(stepData, key);
  }

  // random_int(min,max)
  const intMatch = expr.match(/^random_int\((\d+),\s*(\d+)\)$/);
  if (intMatch) {
    const min = parseInt(intMatch[1]);
    const max = parseInt(intMatch[2]);
    return Math.floor(Math.random() * (max - min + 1)) + min;
  }

  // random_choice(a,b,c)
  const choiceMatch = expr.match(/^random_choice\((.+)\)$/);
  if (choiceMatch) {
    const choices = choiceMatch[1].split(",").map((s) => s.trim());
    return choices[Math.floor(Math.random() * choices.length)];
  }

  // env.VAR_NAME
  if (expr.startsWith(PREFIX_ENV)) {
    return __ENV[expr.substring(PREFIX_ENV.length)] ?? "";
  }

  return undefined;
}

/**
 * Access a nested field on an object via dot-separated path.
 */
function getNestedField(obj, path) {
  if (!path) return obj;
  let current = obj;
  for (const part of path.split(".")) {
    if (current == null) return undefined;
    const idx = parseInt(part);
    current = isNaN(idx) ? current[part] : current[idx];
  }
  return current;
}

/**
 * Template-language truthiness. Falsy: null, undefined, false, 0, NaN,
 * empty string, empty array, empty plain object. Everything else is truthy.
 *
 * Matches Jinja/Liquid/Handlebars conventions more than raw JS truthiness
 * so that an empty search result ({}) or missing extract (undefined) both
 * evaluate falsy in an `if` node's condition.
 */
export function isTruthy(value) {
  if (value == null) return false;
  if (value === false) return false;
  if (typeof value === "number" && (value === 0 || Number.isNaN(value))) return false;
  if (value === "") return false;
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === "object") return Object.keys(value).length > 0;
  return true;
}

/**
 * Resolve a template expression string and apply template-language truthiness.
 * Used by the `if` node type and the `repeat` node's `while` condition.
 */
export function evalTruthy(expr, context) {
  const scopedContext = {
    data: context.data,
    steps: context.steps ?? null,
    env: context.env,
    _dataCache: {},
    _resolveSteps: context._resolveSteps ?? null,
    _dataOverrides: context._dataOverrides ?? null,
  };
  return isTruthy(resolveTemplate(expr, scopedContext));
}

/**
 * Resolve a template expression string and parse as integer. Throws if the
 * resolved value is not a finite integer. Used by `repeat.count_template`.
 */
export function evalInt(expr, context) {
  const scopedContext = {
    data: context.data,
    steps: context.steps ?? null,
    env: context.env,
    _dataCache: {},
    _resolveSteps: context._resolveSteps ?? null,
    _dataOverrides: context._dataOverrides ?? null,
  };
  const resolved = resolveTemplate(expr, scopedContext);
  const n = typeof resolved === "number" ? resolved : parseInt(resolved);
  if (!Number.isFinite(n)) {
    throw new Error(
      `template: expected integer from "${expr}", got ${JSON.stringify(resolved)}`,
    );
  }
  return Math.trunc(n);
}

/**
 * Resolve a single template string. If the entire string is one expression,
 * returns the native type (number, object, etc.). Otherwise performs string
 * interpolation.
 */
export function resolveTemplate(templateStr, context) {
  if (typeof templateStr !== "string") return templateStr;

  // Pass through jsonpath: prefixed strings for post-request processing.
  if (templateStr.startsWith(PREFIX_JSONPATH)) return templateStr;

  // If the entire string is a single expression, return its native type.
  const fullMatch = templateStr.match(/^\{\{(.+?)\}\}$/);
  if (fullMatch && templateStr.indexOf("{{", 2) === -1) {
    return evaluateExpression(fullMatch[1], context);
  }

  // String interpolation: replace all {{...}} with stringified values.
  return templateStr.replace(EXPR_RE, (_, expr) => {
    const value = evaluateExpression(expr, context);
    return value !== undefined ? String(value) : "";
  });
}

/**
 * Deep-resolve all template expressions in an object/array/string.
 * Creates a fresh data cache so that data references within this call
 * are consistent (same collection -> same random item).
 */
export function resolveObject(obj, context) {
  const scopedContext = {
    data: context.data,
    steps: context.steps ?? null,
    env: context.env,
    _dataCache: {},
    _resolveSteps: context._resolveSteps ?? null,
    _dataOverrides: context._dataOverrides ?? null,
  };
  return resolveDeep(obj, scopedContext);
}

function resolveDeep(obj, context) {
  if (obj == null) return obj;
  if (typeof obj === "string") return resolveTemplate(obj, context);
  if (Array.isArray(obj)) return obj.map((item) => resolveDeep(item, context));
  if (typeof obj === "object") {
    const result = {};
    for (const [key, value] of Object.entries(obj)) {
      result[key] = resolveDeep(value, context);
    }
    return result;
  }
  return obj;
}
