// Everything the CLI prints goes through redact: an app key (either
// format), a stream link's capability, a session key, a signed assertion
// or a bearer token never reaches the terminal, and so never an agent's
// transcript, whatever a server or command relayed to us.

const RULES: [RegExp, string][] = [
  // App keys, current and older formats (and anything else wst_-shaped).
  [/wst_[A-Za-z0-9_-]+/g, "wst_[redacted]"],
  // Conversations' session keys.
  [/wsc_[A-Za-z0-9_-]+/g, "wsc_[redacted]"],
  // Capabilities in query strings: stream links, tokens, tickets.
  [/([?&](?:link|token|key|ticket|secret)=)[^&'"\s]+/gi, "$1[redacted]"],
  // Signed assertions and other compact JWS.
  [/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, "[jws]"],
  // Bearer tokens in relayed headers or messages.
  [/(Bearer\s+)[A-Za-z0-9._~+/=-]{8,}/gi, "$1[redacted]"],
];

/** s with every secret-shaped value replaced, and each of secrets too. */
export function redact(s: string, secrets: Iterable<string> = []): string {
  let out = String(s);
  for (const v of secrets) {
    if (v && v.length >= 6) out = out.split(v).join("[redacted]");
  }
  for (const [re, to] of RULES) out = out.replace(re, to);
  return out;
}

/** s redacted and cut to n characters. */
export function clip(s: string, n = 400, secrets: Iterable<string> = []): string {
  const r = redact(s, secrets).replace(/\s+$/, "");
  return r.length > n ? r.slice(0, n) + "…" : r;
}

/** A current-format key anywhere in s (the bench's transcript check uses the same pattern). */
export const KEY_PATTERN = /\bwst_[1-9A-HJ-NP-Za-km-z]{50}\b/;
