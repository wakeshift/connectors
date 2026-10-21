/**
 * An error response from Wakeshift: `code` is the API's error code (e.g.
 * "private_topic", "other_client", "unknown_subscription", "bad_request",
 * "not_headless"), `status` the HTTP status. A request that got no answer
 * has code "unreachable" and status 0.
 */
export class WakeshiftError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string, options?: { cause?: unknown }) {
    super(status ? `wakeshift: ${code || "error"} (${status}): ${message}` : `wakeshift: ${message}`, options);
    this.name = "WakeshiftError";
    this.status = status;
    this.code = code;
  }
}

/** Whether err is a WakeshiftError with code. */
export function isCode(err: unknown, code: string): boolean {
  return err instanceof WakeshiftError && err.code === code;
}
