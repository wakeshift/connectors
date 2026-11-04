// Exit codes, documented so an agent can branch on them.
export const EXIT = {
  /** Done, or every verify check passed. */
  OK: 0,
  /** Something failed: the message says what to fix. */
  FAIL: 1,
  /** The command line was wrong. */
  USAGE: 2,
  /** The account is on the waitlist, or the person isn't signed in. */
  WAITLISTED: 3,
  /** Waited for the console and nothing came. */
  TIMEOUT: 4,
} as const;

export type ExitCode = (typeof EXIT)[keyof typeof EXIT];

/** An error the CLI reports as it is (redacted) and exits with code. */
export class CliError extends Error {
  constructor(
    message: string,
    readonly code: ExitCode = EXIT.FAIL,
  ) {
    super(message);
  }
}

export const usage = (message: string) => new CliError(message, EXIT.USAGE);
export const fail = (message: string) => new CliError(message, EXIT.FAIL);
