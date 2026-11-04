// A small flag parser: --name value, --name=value, and boolean flags.

import { usage } from "./errors.js";

export interface Spec {
  /** Flags that take a value. */
  values: string[];
  /** Flags that don't. */
  booleans: string[];
}

export interface Parsed {
  values: Record<string, string>;
  booleans: Set<string>;
  positionals: string[];
}

export function parseFlags(argv: string[], spec: Spec): Parsed {
  const out: Parsed = { values: {}, booleans: new Set(), positionals: [] };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "--") {
      out.positionals.push(...argv.slice(i + 1));
      break;
    }
    if (!a.startsWith("--")) {
      if (a.startsWith("-") && a !== "-") throw usage(`unknown option ${a.slice(0, 40)}`);
      out.positionals.push(a);
      continue;
    }
    const eq = a.indexOf("=");
    const name = a.slice(2, eq < 0 ? undefined : eq);
    if (spec.booleans.includes(name)) {
      if (eq >= 0) throw usage(`--${name} takes no value`);
      out.booleans.add(name);
      continue;
    }
    if (!spec.values.includes(name)) throw usage(`unknown option --${name.slice(0, 40)}`);
    let v: string | undefined;
    if (eq >= 0) v = a.slice(eq + 1);
    else {
      v = argv[i + 1];
      if (v === undefined || (v.startsWith("--") && v.length > 2)) throw usage(`--${name} needs a value`);
      i++;
    }
    if (name in out.values) throw usage(`--${name} is given twice`);
    out.values[name] = v;
  }
  return out;
}

/** Whether an argument looks like an app key: the CLI never takes one on the command line. */
export const looksLikeKey = (a: string) => /wst_[A-Za-z0-9_-]{8,}/.test(a);
