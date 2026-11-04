import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { inspect } from "node:util";
import { formatKey, generateKey, keyCodeOf, keyIdOf, parseKey, signAssertion } from "../src/key.js";
import { createPublicKey, verify } from "node:crypto";

const here = dirname(fileURLToPath(import.meta.url));
// build-test/test → the package's test/ directory.
const pkg = join(here, "../..");
const vectors = JSON.parse(readFileSync(join(pkg, "test/app-keys.json"), "utf8")) as {
  seeds: { name: string; seed: string; public_key: string; kid: string; key_id: string }[];
  accepted: { key: string; seed: string; mode: string | null; app_id: string | null }[];
  refused: { key: string; why: string }[];
};
const seedOf = (name: string) => vectors.seeds.find((s) => s.name === name)!;

// The shared vectors are the SDKs' (sdk/typescript/test/app-keys.json); the
// copy here goes with the CLI when it's published on its own.
test("the vectors are the SDKs' own", (t) => {
  const shared = join(pkg, "../typescript/test/app-keys.json");
  if (!existsSync(shared)) return t.skip("not in Wakeshift's repository");
  assert.equal(readFileSync(join(pkg, "test/app-keys.json"), "utf8"), readFileSync(shared, "utf8"), "copy sdk/typescript/test/app-keys.json to sdk/cli/test/");
});

test("formatKey writes each seed as the vectors do", () => {
  for (const a of vectors.accepted) {
    if (a.mode !== null || a.key !== a.key.trim()) continue;
    const s = seedOf(a.seed);
    assert.equal(formatKey(Buffer.from(s.seed, "base64url")), a.key, s.name);
  }
});

test("parseKey reads every accepted key, with its public key, kid and key code", () => {
  for (const a of vectors.accepted) {
    const k = parseKey(a.key);
    const s = seedOf(a.seed);
    assert.equal(k.publicKey, s.public_key, a.key);
    assert.equal(k.kid, s.kid);
    assert.equal(k.keyCode, s.key_id);
    assert.equal(keyIdOf(k.publicKeyBytes), s.kid);
    assert.equal(keyCodeOf(k.publicKeyBytes), s.key_id);
    assert.equal(k.appId, a.app_id);
  }
});

test("parseKey refuses every refused key, without quoting it", () => {
  for (const r of vectors.refused) {
    assert.throws(
      () => parseKey(r.key),
      (err: Error) => {
        assert.ok(!r.key.trim() || !err.message.includes(r.key.trim().slice(4)), `${r.why}: the error quotes the key`);
        return true;
      },
      r.why,
    );
  }
});

test("a generated key is a current key with a fresh seed, and round-trips", () => {
  const a = generateKey();
  const b = generateKey();
  assert.match(a.envValue(), /^wst_[1-9A-HJ-NP-Za-km-z]{50}$/);
  assert.notEqual(a.envValue(), b.envValue());
  const again = parseKey(a.envValue());
  assert.equal(again.kid, a.kid);
  assert.equal(again.publicKey, a.publicKey);
  assert.equal(a.keyCode, a.kid.slice(0, 8));
  assert.equal(a.publicKey.length, 43);
});

test("a key never shows itself in its string forms", () => {
  const k = generateKey();
  const secret = k.envValue();
  for (const s of [String(k), `${k}`, JSON.stringify({ k }), inspect(k), inspect({ nested: { k } }, { depth: 5 })]) {
    assert.ok(!s.includes(secret), s);
    assert.ok(!s.includes(secret.slice(4, 20)), s);
  }
});

test("generateKey uses the random source it's given (the OS's CSPRNG by default)", () => {
  const k = generateKey((n) => Buffer.alloc(n, 0));
  assert.equal(k.envValue(), vectors.accepted.find((a) => a.seed === "Z" && a.mode === null)!.key);
  assert.throws(() => generateKey(() => Buffer.alloc(31)));
});

test("an assertion is signed by the key, for the core's audience", () => {
  const k = generateKey();
  const jws = signAssertion(k, "https://core.example", 1_800_000_000_000);
  const [h, c, s] = jws.split(".");
  const header = JSON.parse(Buffer.from(h, "base64url").toString());
  const claims = JSON.parse(Buffer.from(c, "base64url").toString());
  assert.deepEqual(header, { alg: "EdDSA", typ: "app+jwt", kid: k.kid });
  assert.equal(claims.aud, "https://core.example/api/apps");
  assert.equal(claims.iss, k.kid);
  assert.equal(claims.exp - claims.iat, 120);
  const pub = createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: k.publicKey }, format: "jwk" });
  assert.ok(verify(null, Buffer.from(`${h}.${c}`), pub, Buffer.from(s, "base64url")));
  assert.ok(!jws.includes(k.envValue().slice(4)));
});
