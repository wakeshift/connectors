import { createPrivateKey, createPublicKey, randomBytes, sign, type KeyObject } from "node:crypto";

// An app key from the Wakeshift console is the app's Ed25519 private key
// with its ID (docs/apps.md):
//
//   wst_<mode>_<app id>_<seed>
//
// mode is "test" (or, later, "live"); the app ID never contains "_"; the
// seed is 32 bytes, base64url without padding (43 characters, which may
// include "_" and "-"). The key never goes over the wire: it signs
// short-lived assertions, and Wakeshift keeps only the public half.

export interface AppKey {
  mode: "test" | "live";
  appId: string;
  privateKey: KeyObject;
}

const appIdRE = /^[a-z][a-z0-9-]{1,39}$/;
const seedRE = /^[A-Za-z0-9_-]{43}$/;
// PKCS #8 for an Ed25519 key, before its 32-byte seed (RFC 8410).
const pkcs8Prefix = Buffer.from("302e020100300506032b657004220420", "hex");

/** Reads an app key from the console. Throws on anything else. */
export function parseKey(key: string): AppKey {
  const s = key.trim();
  if (!s.startsWith("wst_")) throw new Error("wakeshift: an app key starts with wst_");
  const rest = s.slice(4);
  const i = rest.indexOf("_");
  const mode = i < 0 ? rest : rest.slice(0, i);
  if (mode !== "test" && mode !== "live") throw new Error("wakeshift: an app key is wst_test_… or wst_live_…");
  const after = rest.slice(i + 1);
  const j = after.indexOf("_");
  const appId = j < 0 ? "" : after.slice(0, j);
  if (!appIdRE.test(appId)) throw new Error("wakeshift: the app key has no app ID");
  const seed = after.slice(j + 1);
  const raw = seedRE.test(seed) ? Buffer.from(seed, "base64url") : Buffer.alloc(0);
  if (raw.length !== 32) throw new Error("wakeshift: the app key is cut short or mistyped");
  const privateKey = createPrivateKey({ key: Buffer.concat([pkcs8Prefix, raw]), format: "der", type: "pkcs8" });
  return { mode, appId, privateKey };
}

/** The app's public key, base64url (what the console stores). */
export function publicKeyOf(k: AppKey): string {
  const jwk = createPublicKey(k.privateKey).export({ format: "jwk" });
  return jwk.x as string;
}

const b64 = (v: unknown) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");

/**
 * Signs a fresh assertion of the app's identity for core (its base URL,
 * without a trailing slash): a compact JWS, alg EdDSA, typ app+jwt, living
 * two minutes.
 */
export function signAssertion(k: AppKey, core: string, now = Date.now()): string {
  const iat = Math.floor(now / 1000);
  const claims = {
    iss: k.appId,
    sub: k.appId,
    aud: core + "/api/apps",
    iat,
    exp: iat + 120,
    jti: randomBytes(16).toString("base64url"),
  };
  const input = b64({ alg: "EdDSA", typ: "app+jwt" }) + "." + b64(claims);
  return input + "." + sign(null, Buffer.from(input), k.privateKey).toString("base64url");
}
