// App keys (docs/api-keys.md): the app's Ed25519 private key, as
//
//   wst_<50 base58 characters>
//
// the 32-byte seed then its CRC32 (IEEE, big-endian), read as one
// big-endian integer and written in base58 (Bitcoin's alphabet),
// left-padded with "1". Older keys, wst_<test|live>_<app id>_<seed in
// base64url>, are read too. The same rules as internal/appkey and the SDKs,
// tested against their shared vectors (test/app-keys.json).
//
// The key is made here, on the developer's machine, written to .env and
// never printed: an AppKey's string forms are "[app key]", and only
// envValue() gives the key itself, for the file.

import { createHash, createPrivateKey, createPublicKey, randomBytes, sign, type KeyObject } from "node:crypto";
import { inspect } from "node:util";

const ALPHABET = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";
const LEN = 50;
const LIMIT = 1n << 288n;
const APP_ID = /^[a-z][a-z0-9-]{1,39}$/;
const SEED_B64 = /^[A-Za-z0-9_-]{43}$/;
// PKCS #8 for an Ed25519 key, before its 32-byte seed (RFC 8410).
const PKCS8_PREFIX = Buffer.from("302e020100300506032b657004220420", "hex");

// Errors never include the key.
export const ERR_PREFIX = "an app key starts with wst_";
export const ERR_FORMAT = "an app key is wst_ followed by 50 letters and digits (or an older wst_test_… or wst_live_… key)";
export const ERR_NO_APP_ID = "the app key has no app ID";
export const ERR_MISTYPED = "the app key is cut short or mistyped";

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

/** CRC32 (IEEE). */
export function crc32(b: Uint8Array): number {
  let c = 0xffffffff;
  for (const x of b) c = CRC_TABLE[(c ^ x) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** A public key's kid: base64url of the first 16 bytes of its SHA-256 (22 characters). */
export function keyIdOf(publicKey: Uint8Array): string {
  return createHash("sha256").update(publicKey).digest().subarray(0, 16).toString("base64url");
}

/** The key code the console shows as key_id: the kid's first 8 characters. */
export function keyCodeOf(publicKey: Uint8Array): string {
  return keyIdOf(publicKey).slice(0, 8);
}

/** Writes a current-format key for a 32-byte seed (appkey.Format). */
export function formatKey(seed: Uint8Array): string {
  if (seed.length !== 32) throw new Error("a seed is 32 bytes");
  const b = Buffer.alloc(36);
  Buffer.from(seed).copy(b, 0);
  b.writeUInt32BE(crc32(seed), 32);
  let n = BigInt("0x" + b.toString("hex"));
  const out: string[] = new Array(LEN);
  for (let i = LEN - 1; i >= 0; i--) {
    out[i] = ALPHABET[Number(n % 58n)];
    n /= 58n;
  }
  return "wst_" + out.join("");
}

/** An app key, held so that it can't be printed by accident. */
export class AppKey {
  readonly #seed: Buffer;
  readonly #text: string;
  readonly #private: KeyObject;
  /** The app's ID, for an older key that names it. */
  readonly appId: string | null;
  /** The raw 32-byte public key. */
  readonly publicKeyBytes: Buffer;
  /** The public key, base64url: what the console registers. */
  readonly publicKey: string;
  /** The key's kid, in every assertion's header. */
  readonly kid: string;
  /** The console's key_id: the kid's first 8 characters. */
  readonly keyCode: string;

  constructor(seed: Uint8Array, text: string, appId: string | null = null) {
    this.#seed = Buffer.from(seed);
    this.#text = text;
    this.appId = appId;
    this.#private = createPrivateKey({ key: Buffer.concat([PKCS8_PREFIX, this.#seed]), format: "der", type: "pkcs8" });
    const jwk = createPublicKey(this.#private).export({ format: "jwk" });
    this.publicKeyBytes = Buffer.from(String(jwk.x), "base64url");
    this.publicKey = this.publicKeyBytes.toString("base64url");
    this.kid = keyIdOf(this.publicKeyBytes);
    this.keyCode = this.kid.slice(0, 8);
  }

  /** The key itself, only for writing to the env file. Never print it. */
  envValue(): string {
    return this.#text;
  }

  /** Signs bytes with the key (Ed25519). */
  sign(data: Uint8Array): Buffer {
    return sign(null, data, this.#private);
  }

  /** Whether two keys are the same key. */
  same(other: AppKey): boolean {
    return this.kid === other.kid;
  }

  toString(): string {
    return "[app key]";
  }
  toJSON(): string {
    return "[app key]";
  }
  [inspect.custom](): string {
    return `AppKey(${this.keyCode})`;
  }
}

/** A new key, from the OS's CSPRNG (or rand, in tests). */
export function generateKey(rand: (n: number) => Buffer = randomBytes): AppKey {
  const seed = rand(32);
  if (seed.length !== 32) throw new Error("the random source gave the wrong number of bytes");
  return new AppKey(seed, formatKey(seed));
}

/** Reads an app key, either format. Throws an Error whose message never includes the key. */
export function parseKey(key: string): AppKey {
  const s = String(key ?? "").trim();
  if (!s.startsWith("wst_")) throw new Error(ERR_PREFIX);
  const rest = s.slice(4);
  const i = rest.indexOf("_");
  if (i >= 0) {
    const mode = rest.slice(0, i);
    const after = rest.slice(i + 1);
    if (mode !== "test" && mode !== "live") throw new Error(ERR_FORMAT);
    const j = after.indexOf("_");
    const appId = j < 0 ? "" : after.slice(0, j);
    if (!APP_ID.test(appId)) throw new Error(ERR_NO_APP_ID);
    const enc = after.slice(j + 1);
    const raw = SEED_B64.test(enc) ? Buffer.from(enc, "base64url") : Buffer.alloc(0);
    if (raw.length !== 32) throw new Error(ERR_MISTYPED);
    return new AppKey(raw, s, appId);
  }
  if (rest.length !== LEN) throw new Error(ERR_MISTYPED);
  let n = 0n;
  for (const ch of rest) {
    const d = ALPHABET.indexOf(ch);
    if (d < 0) throw new Error(ERR_MISTYPED);
    n = n * 58n + BigInt(d);
  }
  if (n >= LIMIT) throw new Error(ERR_MISTYPED);
  const b = Buffer.from(n.toString(16).padStart(72, "0"), "hex");
  const seed = b.subarray(0, 32);
  if (crc32(seed) !== b.readUInt32BE(32)) throw new Error(ERR_MISTYPED);
  return new AppKey(seed, s);
}

const b64 = (v: unknown) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");

/**
 * A fresh assertion of the app's identity for core (its origin): a compact
 * JWS, alg EdDSA, typ app+jwt, with the key's kid, living two minutes, as
 * the SDKs sign it. iss and sub are the app's ID when known, else the kid.
 */
export function signAssertion(k: AppKey, core: string, now = Date.now(), appId?: string | null): string {
  const iat = Math.floor(now / 1000);
  const who = appId || k.appId || k.kid;
  const claims = { iss: who, sub: who, aud: core + "/api/apps", iat, exp: iat + 120, jti: randomBytes(16).toString("base64url") };
  const input = b64({ alg: "EdDSA", typ: "app+jwt", kid: k.kid }) + "." + b64(claims);
  return input + "." + k.sign(Buffer.from(input)).toString("base64url");
}
