// Type-checks every TypeScript example people copy against this SDK: the
// ```ts blocks in site/docs and this README, the landing page's sample
// (site/templates/landing.html) and the console's "Try it" snippet
// (web/src/pages/app/Overview.tsx). Each example becomes a module: its
// imports at the top, the rest in an async function, so fragments (a
// `return` from a tool callback, an `await`) compile; examples/env.d.ts
// declares what fragments take from around them (extra, repo, body…).
// It also checks three mistakes tsc can't see: a test key needs `core`,
// ws.call needs `server` (or `client`), and topics passed to the SDK are
// lowercase (a topic in a variable isn't checked).
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync, copyFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";

const sdk = join(dirname(fileURLToPath(import.meta.url)), "..");
const repo = join(sdk, "../..");
const out = join(sdk, "build-examples");

const examples = []; // { from, code }

// ```ts blocks in Markdown (the info string may add a title or tab: "ts server.ts").
function fromMarkdown(file) {
  const src = readFileSync(file, "utf8");
  const re = /^```(ts|typescript)\b[^\n]*\n([\s\S]*?)^```/gm;
  let m;
  while ((m = re.exec(src))) {
    const line = src.slice(0, m.index).split("\n").length;
    examples.push({ from: `${relative(repo, file)}:${line}`, code: m[2] });
  }
}
// WAKESHIFT_EXAMPLES_README_ONLY=1 checks only this README: the published
// copy of the SDK (opensource/export.sh) has no site or console.
const readmeOnly = process.env.WAKESHIFT_EXAMPLES_README_ONLY === "1";
fromMarkdown(join(sdk, "README.md"));
if (!readmeOnly) {
  const docs = join(repo, "site/docs");
  for (const f of readdirSync(docs).filter((f) => f.endsWith(".md")).sort()) fromMarkdown(join(docs, f));
}

// The landing page's sample: the <pre> in the code card, without its highlighting.
if (!readmeOnly) {
  const html = readFileSync(join(repo, "site/templates/landing.html"), "utf8");
  const m = html.match(/<div class="code">[\s\S]*?<pre>([\s\S]*?)<\/pre>/);
  if (!m) throw new Error("landing.html: no code sample");
  const code = m[1]
    .replace(/<span class="ln">/g, "\n")
    .replace(/<[^>]+>/g, "")
    .replace(/&gt;/g, ">")
    .replace(/&lt;/g, "<")
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, "&");
  examples.push({ from: "site/templates/landing.html", code });
}

// The console's snippet: the template literal from "// server.ts", with this app's ID.
if (!readmeOnly) {
  const tsx = readFileSync(join(repo, "web/src/pages/app/Overview.tsx"), "utf8");
  const m = tsx.match(/\n\/\/ server\.ts\n([\s\S]*?)`\n\s*return \(/);
  if (!m) throw new Error("Overview.tsx: no server.ts snippet");
  const code = m[1].replace(/\$\{app\.id\}/g, "ci-bot").replace(/\\`/g, "`").replace(/\\\$\{/g, "${");
  if (/\$\{app\./.test(code)) throw new Error("Overview.tsx: the snippet uses an app field this check doesn't fill in");
  examples.push({ from: "web/src/pages/app/Overview.tsx", code });
}

// What tsc can't see.
const problems = [];
for (const e of examples) {
  for (const m of e.code.matchAll(/new Wakeshift\(\{([^}]*)\}\)/g)) {
    if (!/\bcore\b/.test(m[1])) problems.push(`${e.from}: new Wakeshift without core (a test key needs it)`);
  }
  // ws.call throws without the client's name.
  for (const m of e.code.matchAll(/\bws\.call\(\s*\w+\s*(?:,\s*\{([^}]*)\})?\s*\)/g)) {
    if (!/\b(server|client)\b/.test(m[1] ?? "")) problems.push(`${e.from}: ws.call without server or client (it throws)`);
  }
  for (const t of topicArgs(e.code)) {
    const why = topicProblem(t);
    if (why) problems.push(`${e.from}: topic ${t} ${why}`);
  }
}

// topicArgs is every topic an example passes the SDK: the topic argument of
// subscribe, unsubscribe, unsubscribeUser, publish (one, or each in an
// array), publishUrl, closeTopic and topicStatus, as written (a string or
// template literal; a variable isn't checked).
function topicArgs(code) {
  const out = [];
  const calls = /\bws\.(subscribe|unsubscribe|unsubscribeUser|publish|publishUrl|closeTopic|topicStatus)\(/g;
  let m;
  while ((m = calls.exec(code))) {
    const args = splitArgs(code, m.index + m[0].length);
    const arg = ["subscribe", "unsubscribe", "unsubscribeUser"].includes(m[1]) ? args[1] : args[0];
    if (!arg) continue;
    const literals = arg.startsWith("[") ? splitArgs(arg, 1) : [arg];
    out.push(...literals.filter((l) => /^["'`]/.test(l)));
  }
  return out;
}

// splitArgs splits the arguments of a call whose "(" (or "[") ends just
// before i, at top-level commas, skipping strings, templates and brackets.
function splitArgs(code, i) {
  const args = [];
  let depth = 0;
  let start = i;
  for (; i < code.length; i++) {
    const c = code[i];
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < code.length && code[i] !== c; i++) {
        if (code[i] === "\\") i++;
        else if (c === "`" && code[i] === "$" && code[i + 1] === "{") {
          let d = 0;
          for (; i < code.length; i++) {
            if (code[i] === "{") d++;
            else if (code[i] === "}" && --d === 0) break;
          }
        }
      }
    } else if ("([{".includes(c)) depth++;
    else if (")]}".includes(c)) {
      if (depth-- === 0) break;
    } else if (c === "," && depth === 0) {
      args.push(code.slice(start, i).trim());
      start = i + 1;
    }
  }
  args.push(code.slice(start, i).trim());
  return args.filter((a) => a !== "");
}

// topicProblem says what's wrong with a topic literal: its fixed parts must
// be lowercase topic characters, and each value in it must be lowercased
// with .toLowerCase() (or be an ID, which the docs say must already be
// topic-safe: u/${me.id}/…).
function topicProblem(t) {
  const fixed = t.slice(1, -1).replace(/\$\{[^}]*\}/g, "x");
  if (!/^[a-z0-9._-]+(\/[a-z0-9._-]+)*$/.test(fixed)) return "has characters a topic can't (lowercase a-z 0-9 . _ - joined by /)";
  for (const v of t.matchAll(/\$\{([^}]*)\}/g)) {
    if (!/\.toLowerCase\(\)\s*$/.test(v[1]) && !/(^|\.)id$/i.test(v[1].trim())) return "isn't lowercased (use .toLowerCase() on " + v[1].trim() + ")";
  }
  return "";
}

rmSync(out, { recursive: true, force: true });
mkdirSync(out, { recursive: true });
copyFileSync(join(sdk, "scripts/examples/env.d.ts"), join(out, "env.d.ts"));
const files = [];
examples.forEach((e, i) => {
  const lines = e.code.split("\n");
  const imports = lines.filter((l) => /^import\s/.test(l));
  const rest = lines.filter((l) => !/^import\s/.test(l));
  const name = `${String(i).padStart(2, "0")}-${e.from.replace(/[^a-z0-9]+/gi, "-").replace(/^-|-$/g, "")}.ts`;
  writeFileSync(
    join(out, name),
    `// ${e.from}\n${imports.join("\n")}\nexport {};\n\nexport async function example() {\n${rest.join("\n")}\n}\n`,
  );
  files.push(name);
});
writeFileSync(
  join(out, "tsconfig.json"),
  JSON.stringify(
    {
      compilerOptions: {
        target: "ES2023",
        module: "NodeNext",
        moduleResolution: "NodeNext",
        lib: ["ES2023"],
        types: ["node"],
        strict: true,
        noEmit: true,
        skipLibCheck: true,
        paths: { "@wakeshift/sdk": ["../src/index.ts"] },
      },
      files: ["env.d.ts", ...files],
    },
    null,
    2,
  ),
);

let failed = false;
try {
  execFileSync(join(sdk, "node_modules/.bin/tsc"), ["-p", join(out, "tsconfig.json")], { stdio: "inherit" });
} catch {
  failed = true;
}
for (const p of problems) console.error(p);
if (failed || problems.length) {
  console.error(`examples: ${problems.length} problem(s)${failed ? ", and tsc errors above" : ""} (the files are in ${relative(process.cwd(), out)})`);
  process.exit(1);
}
rmSync(out, { recursive: true, force: true });
console.log(`examples: ${examples.length} type-check against the SDK`);
