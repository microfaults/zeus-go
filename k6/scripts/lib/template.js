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

/**
 * Evaluate a single template expression against the context.
 */
function evaluateExpression(expr, context) {
  expr = expr.trim();

  // data.<collection>.<field>
  if (expr.startsWith("data.")) {
    const parts = expr.split(".");
    const collection = parts[1];
    const field = parts.slice(2).join(".");

    // Check variant overrides first
    if (context._dataOverrides) {
      const overridePath = collection + (field ? "." + field : "");
      const override = getNestedField(context._dataOverrides, overridePath);
      if (override !== undefined) return override;
    }

    if (!context._dataCache[collection]) {
      const items = context.data ? context.data[collection] : undefined;
      if (!items || items.length === 0) {
        return undefined;
      }
      context._dataCache[collection] =
        items[Math.floor(Math.random() * items.length)];
    }

    const item = context._dataCache[collection];
    return getNestedField(item, field);
  }

  // steps.<id>.<key> — scope-chain resolution in v2, flat lookup fallback
  if (expr.startsWith("steps.")) {
    const path = expr.substring(6);
    if (context._resolveSteps) {
      return context._resolveSteps(path);
    }
    const parts = path.split(".");
    const stepName = parts[0];
    const key = parts.slice(1).join(".");
    const stepData = context.steps ? context.steps[stepName] : undefined;
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
  if (expr.startsWith("env.")) {
    const varName = expr.substring(4);
    return __ENV[varName] || "";
  }

  return undefined;
}

/**
 * Access a nested field on an object via dot-separated path.
 */
function getNestedField(obj, path) {
  if (!path) return obj;
  const parts = path.split(".");
  let current = obj;
  for (const part of parts) {
    if (current === null || current === undefined) return undefined;
    const idx = parseInt(part);
    current = isNaN(idx) ? current[part] : current[idx];
  }
  return current;
}

/**
 * Resolve a single template string. If the entire string is one expression,
 * returns the native type (number, object, etc.). Otherwise performs string
 * interpolation.
 */
export function resolveTemplate(templateStr, context) {
  if (typeof templateStr !== "string") return templateStr;

  // Pass through jsonpath: prefixed strings for post-request processing
  if (templateStr.startsWith("jsonpath:")) return templateStr;

  // If the entire string is a single expression, return its native type
  const fullMatch = templateStr.match(/^\{\{(.+?)\}\}$/);
  if (fullMatch && templateStr.indexOf("{{", 2) === -1) {
    return evaluateExpression(fullMatch[1], context);
  }

  // String interpolation: replace all {{...}} with stringified values
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
    steps: context.steps || null,
    env: context.env,
    _dataCache: {},
    _resolveSteps: context._resolveSteps || null,
    _dataOverrides: context._dataOverrides || null,
  };
  return resolveDeep(obj, scopedContext);
}

function resolveDeep(obj, context) {
  if (obj === null || obj === undefined) return obj;
  if (typeof obj === "string") return resolveTemplate(obj, context);
  if (Array.isArray(obj)) return obj.map((item) => resolveDeep(item, context));
  if (typeof obj === "object") {
    const result = {};
    for (const key of Object.keys(obj)) {
      result[key] = resolveDeep(obj[key], context);
    }
    return result;
  }
  return obj;
}
