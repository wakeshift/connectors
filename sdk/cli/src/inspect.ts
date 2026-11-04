// What verify reads from a server's answers: the sentence tools/list adds
// to a tool that declares the events it subscribes to, a listed
// subscription, and a payload that fits an event's schema.

import type { EventDef, JsonSchema } from "./core.js";

export interface NotifiesSentence {
  /** The event descriptions it names (subscribes); empty for notifies() with its own wording. */
  descriptions: string[];
}

/**
 * The notifies sentence in a tool's description (sdk/typescript/test/subscribes.json:
 * "This tool is how <app> notifies you in this conversation[ of this event: <d>| of these events: <d1>; <d2>]. <App> delivers…"),
 * or null.
 */
export function notifiesSentence(description: string): NotifiesSentence | null {
  const d = description.replace(/\s+/g, " ");
  const m = /This tool is how .+? notifies you in this conversation(.*?)\. [^.]*? delivers its notifications through Wakeshift/.exec(d);
  if (!m) return null;
  const tail = m[1];
  const ev = /^ of (?:this event|these events): (.+)$/.exec(tail);
  return { descriptions: ev ? ev[1].split("; ").map(norm).filter(Boolean) : [] };
}

/** A description as the sentence writes it: one line, no trailing periods or spaces. */
export const norm = (s: string) =>
  s
    .replace(/\s+/g, " ")
    .trim()
    .replace(/[.\s]+$/, "");

/** The manifest's events a sentence names, by description. */
export function eventsNamed(s: NotifiesSentence, events: EventDef[]): EventDef[] {
  return events.filter((e) => e.description && s.descriptions.includes(norm(e.description)));
}

/** A subscription list_subscriptions lists: "- <id>: <event> <arguments>, <destination>, since <since>". */
export interface Listed {
  id: string;
  event: string;
  arguments: Record<string, unknown>;
}

export function listedSubscriptions(text: string): Listed[] {
  const out: Listed[] = [];
  for (const line of text.split("\n")) {
    const m = /^- ([^:\s]+): (\S+) (\{.*\}), /.exec(line.trim());
    if (!m) continue;
    try {
      const args = JSON.parse(m[3]);
      if (args && typeof args === "object" && !Array.isArray(args)) out.push({ id: m[1], event: m[2], arguments: args });
    } catch {
      // arguments that aren't JSON: not one verify can use
    }
  }
  return out;
}

/** The event's arguments that args gives values for (by the schema's property names). */
export function argsForEvent(ev: EventDef, args: Record<string, unknown>): { picked: Record<string, unknown>; missing: string[] } {
  const props = Object.keys(ev.arguments?.properties ?? {});
  const required: string[] = Array.isArray(ev.arguments?.required) ? ev.arguments!.required : [];
  const picked: Record<string, unknown> = {};
  for (const p of props) if (p in args) picked[p] = args[p];
  return { picked, missing: required.filter((r) => !(r in picked)) };
}

/** A value that fits schema, as small as it can be, with marker in its strings. */
export function sample(schema: JsonSchema | undefined, marker: string, depth = 0): unknown {
  if (!schema || typeof schema !== "object" || depth > 8) return {};
  if ("const" in schema) return schema.const;
  if (Array.isArray(schema.enum) && schema.enum.length) return schema.enum[0];
  if ("default" in schema) return schema.default;
  for (const k of ["anyOf", "oneOf"]) {
    if (Array.isArray(schema[k])) {
      const first = schema[k].find((s: JsonSchema) => s?.type !== "null") ?? schema[k][0];
      return sample(first, marker, depth + 1);
    }
  }
  if (Array.isArray(schema.allOf) && schema.allOf.length) return sample(Object.assign({}, ...schema.allOf), marker, depth + 1);
  const type = Array.isArray(schema.type) ? (schema.type.find((t: string) => t !== "null") ?? schema.type[0]) : schema.type;
  switch (type) {
    case "string": {
      if (schema.format === "date-time") return new Date().toISOString();
      if (schema.format === "date") return new Date().toISOString().slice(0, 10);
      if (schema.format === "uri" || schema.format === "url") return `https://example.com/${marker}`;
      if (schema.format === "email") return `${marker}@example.com`;
      if (schema.format === "uuid") return "00000000-0000-4000-8000-000000000000";
      let s = marker;
      if (typeof schema.maxLength === "number" && s.length > schema.maxLength) s = s.slice(0, schema.maxLength);
      if (typeof schema.minLength === "number" && s.length < schema.minLength) s = s.padEnd(schema.minLength, "x");
      return s;
    }
    case "integer":
    case "number": {
      // The value nearest 0 inside the bounds (exclusive ones by a step).
      const step = type === "integer" ? 1 : 0.5;
      const num = (v: unknown) => (typeof v === "number" ? v : undefined);
      const lo = num(schema.minimum) ?? (num(schema.exclusiveMinimum) !== undefined ? schema.exclusiveMinimum + step : undefined);
      const hi = num(schema.maximum) ?? (num(schema.exclusiveMaximum) !== undefined ? schema.exclusiveMaximum - step : undefined);
      let v = 0;
      if (lo !== undefined && v < lo) v = lo;
      if (hi !== undefined && v > hi) v = hi;
      return type === "integer" ? (lo !== undefined && v === lo ? Math.ceil(v) : Math.floor(v)) : v;
    }
    case "boolean":
      return false;
    case "null":
      return null;
    case "array": {
      const n = typeof schema.minItems === "number" ? schema.minItems : 0;
      return Array.from({ length: n }, () => sample(schema.items, marker, depth + 1));
    }
    default: {
      const props: Record<string, JsonSchema> = schema.properties ?? {};
      const required: string[] = Array.isArray(schema.required) ? schema.required : [];
      const out: Record<string, unknown> = {};
      for (const k of required) out[k] = sample(props[k], marker, depth + 1);
      return out;
    }
  }
}

export type Trigger = { kind: "synthetic" } | { kind: "tool"; name: string; args: Record<string, unknown> } | { kind: "cmd"; command: string };

/** --trigger: tool:<name>[:<json>], cmd:<command>, or none (verify emits a synthetic occurrence). */
export function parseTrigger(s: string | undefined): Trigger | string {
  if (s === undefined) return { kind: "synthetic" };
  if (s.startsWith("tool:")) {
    const rest = s.slice(5);
    const i = rest.indexOf(":");
    const name = (i < 0 ? rest : rest.slice(0, i)).trim();
    if (!name) return "--trigger tool: needs a tool's name (tool:<name>[:<json arguments>])";
    if (i < 0) return { kind: "tool", name, args: {} };
    try {
      const args = JSON.parse(rest.slice(i + 1));
      if (!args || typeof args !== "object" || Array.isArray(args)) return "--trigger tool:<name>:<json>: the arguments are a JSON object";
      return { kind: "tool", name, args };
    } catch {
      return "--trigger tool:<name>:<json>: the arguments aren't valid JSON";
    }
  }
  if (s.startsWith("cmd:")) {
    const command = s.slice(4).trim();
    if (!command) return "--trigger cmd: needs a command (cmd:<command>)";
    return { kind: "cmd", command };
  }
  return "--trigger is tool:<name>[:<json>] or cmd:<command>";
}

/** Canonical JSON (keys sorted), as Wakeshift's own messages write arguments. */
export function canonical(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canonical).join(",")}]`;
  if (v && typeof v === "object")
    return `{${Object.keys(v as object)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${canonical((v as Record<string, unknown>)[k])}`)
      .join(",")}}`;
  return JSON.stringify(v);
}
