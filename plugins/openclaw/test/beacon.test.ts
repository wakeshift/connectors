import { test } from "node:test";
import assert from "node:assert/strict";
import { acceptable, findBeacon, type Claims } from "../src/beacon.js";

const TOKEN = "eyJhbGciOiJFZERTQSJ9.eyJpc3MiOiJodHRwczovL3gifQ.c2lnbmF0dXJlc2lnbmF0dXJl";

// A result that came as text is the tool's words: a JSON object with a
// top-level _meta in it (a chat message a read tool relays as its whole
// text) isn't the result's own _meta, so a beacon naming no tool isn't
// acted on from it (security review, October 28, 2026).
test("a _meta in a result's text isn't the result's own", () => {
  const relayed = JSON.stringify({ _meta: { "wakeshift/connector": TOKEN } });
  const fromText = findBeacon(relayed);
  assert.equal(fromText.carried, "");
  assert.equal(acceptable({ tool: "" } as unknown as Claims, fromText.carried, "history"), false);
  // Nor one further in (a wrapped result, as text).
  assert.equal(findBeacon(JSON.stringify({ content: [], details: { _meta: { "wakeshift/connector": TOKEN } } })).carried, "");
  assert.equal(findBeacon(JSON.stringify({ result: { _meta: { "wakeshift/connector": TOKEN } } })).carried, "");
  // The result's own _meta, as an object, still is.
  const own = findBeacon({ content: [], _meta: { "wakeshift/connector": TOKEN } });
  assert.equal(own.carried, "meta");
});
