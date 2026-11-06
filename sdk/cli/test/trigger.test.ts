// Substituting values from the subscribing tool's reply into a trigger.

import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { test } from "node:test";
import { resultJson, shellQuote, substitute } from "../src/inspect.js";
import { redact } from "../src/redact.js";

const secret = (s: string) => redact(s) !== s;

test("resultJson: a JSON text block, or a string result's text before Wakeshift's block", () => {
  assert.deepEqual(resultJson(['{"number":7}', "Wakeshift: …"]), { number: 7 });
  assert.deepEqual(resultJson(['{"a":\n\n1}\n\nWakeshift: start the Monitor']), { a: 1 });
  assert.equal(resultJson(["Started build #7.", "Wakeshift"]), undefined);
});

test("substitute: whole-string placeholders keep their type; others are text; shell values are quoted", () => {
  const values = { result: { number: 7, items: [{ id: "b-1" }] }, structured: { run: "it's; echo pwned" } };
  const used = new Map<string, unknown>();
  assert.deepEqual(substitute({ number: "{result.number}", label: "build {result.number}", id: "{result.items.0.id}", n: 3 }, values, used, secret), { number: 7, label: "build 7", id: "b-1", n: 3 });
  assert.deepEqual([...used.keys()].sort(), ["{result.items.0.id}", "{result.number}"]);
  const cmd = substitute("printf %s {structured.run}", values, new Map(), secret, true) as string;
  assert.equal(cmd, `printf %s ${shellQuote("it's; echo pwned")}`);
  if (process.platform !== "win32") assert.equal(spawnSync("sh", ["-c", cmd], { encoding: "utf8" }).stdout, "it's; echo pwned");
  // Text without placeholders, or with other braces, is unchanged.
  assert.equal(substitute("curl -d '{\"a\":1}' ${HOME}", values, new Map(), secret, true), "curl -d '{\"a\":1}' ${HOME}");
});

test("substitute: a missing value or a secret-shaped one is refused", () => {
  assert.throws(() => substitute("{result.nope}", { result: { a: 1 } }, new Map(), secret), /\{result\.nope\}: the tool's reply has no nope/);
  assert.throws(() => substitute("{structured.a}", { result: { a: 1 } }, new Map(), secret), /no structuredContent/);
  assert.throws(() => substitute("{result.a}", {}, new Map(), secret), /no JSON in its text/);
  assert.throws(() => substitute("x {result.k}", { result: { k: "wsc_abcdefgh" } }, new Map(), secret), /looks like a key/);
  assert.throws(() => substitute("{result.k}", { result: { k: "https://h/v1/stream/wait?link=abc" } }, new Map(), secret), /looks like a key/);
});
