#!/usr/bin/env node
// wakeshift-cli: Wakeshift's command line for developers. Run it with
// npx -y @wakeshift/cli@<version> <command>.

import { realpathSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { looksLikeKey } from "./args.js";
import { type Ctx, Out, realCtx } from "./context.js";
import { CliError, EXIT } from "./errors.js";
import { redact } from "./redact.js";
import { init, keysNew, status } from "./setup.js";
import { VERSION } from "./version.js";
import { verify } from "./verify.js";

export const HELP = `wakeshift-cli ${VERSION}: add Wakeshift events to your MCP server.

Usage: npx -y @wakeshift/cli@${VERSION} <command> [options]

Commands:
  init        Make an app key on this computer, write it to .env, and create the
              app: open the link it prints, sign in and confirm.
                --name <id>             the app's ID (default: <project>-dev)
                --display-name <name>   the name agents see (default: from the project)
                --env <path>            where to write WAKESHIFT_KEY (default: .env)
                --replace               replace a WAKESHIFT_KEY that's already there
  keys new    Make a new key for an existing app, and add it with a console link.
                --app <id>              the app (default: the app of the key in .env)
                --env <path>
  status      The app, its key, and the events Wakeshift has from your server.
                --env <path>  --json
  verify      Act as an agent: call the tool that subscribes, trigger an event,
              wait for the wake, clean up, and report what to fix.
                --url <mcp url>         your running server (http://localhost:3000/mcp)
                --tool <name>           the tool that subscribes the conversation
                --args <json>           its arguments
                --trigger <how>         tool:<name>[:<json>] or cmd:<command> to make the
                                        event happen (default: verify emits a test event);
                                        {result.<path>} or {structured.<path>} in it is a
                                        value from the tool's reply ({result.number})
                --header-env <NAME>     send Authorization: Bearer $NAME, for a server
                                        with its own sign-in
                --event <name>          which event, if the tool declares several
                --event-args <json>     the arguments the tool subscribes with, if verify
                                        can't tell
                --user <id>             the test user's ID, for a personal event
                --payload <json>        the test event's payload
                --timeout <seconds>     how long to wait for the wake (default 30)
                --live-app              allow an app whose ID doesn't end in -dev or -test
                --json                  the report as JSON

More options:
  --origin <url>   another Wakeshift (default https://app.wakeshift.ai; or WAKESHIFT_CORE)
  --no-browser     print the link without opening it (init, keys new)
  --wait <seconds> how long init and keys new wait for the confirmation (default 600)

The key is made on this computer and never printed: not here, not in a link, never
as an argument. Don't read or print .env; use status and verify to check the key.

Exit codes: 0 done (verify: passed), 1 failed, 2 usage, 3 waitlisted or not signed
in, 4 timed out.
`;

/** Runs a command line; returns the exit code. Never prints a key, whatever goes wrong. */
export async function main(argv: string[], ctx: Ctx): Promise<number> {
  const out = new Out(ctx);
  if (argv.some(looksLikeKey)) {
    out.err("wakeshift-cli never takes an app key on the command line: it makes the key itself and keeps it in .env. Remove it from the command (and from your shell history).");
    return EXIT.USAGE;
  }
  const [cmd, ...rest] = argv;
  try {
    switch (cmd) {
      case "init":
        return await init(ctx, rest);
      case "keys":
        if (rest[0] === "new") return await keysNew(ctx, rest.slice(1));
        out.err(`usage: wakeshift-cli keys new [--app <id>] [--env <path>]`);
        return EXIT.USAGE;
      case "status":
        return await status(ctx, rest);
      case "verify":
        return await verify(ctx, rest);
      case "--version":
      case "-v":
      case "version":
        out.line(VERSION);
        return EXIT.OK;
      case undefined:
      case "help":
      case "--help":
      case "-h":
        out.line(HELP.trimEnd());
        return cmd === undefined ? EXIT.USAGE : EXIT.OK;
      default:
        out.err(`wakeshift-cli: unknown command ${String(cmd).slice(0, 40)}. Run wakeshift-cli --help.`);
        return EXIT.USAGE;
    }
  } catch (err) {
    const code = err instanceof CliError ? err.code : EXIT.FAIL;
    const msg = err instanceof Error ? err.message : String(err);
    if (rest.includes("--json")) out.line(JSON.stringify({ ok: false, error: redact(msg) }));
    else out.err(`wakeshift-cli: ${msg}${code === EXIT.USAGE ? " (wakeshift-cli --help)" : ""}`);
    return code;
  }
}

function isMain(): boolean {
  try {
    return !!process.argv[1] && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isMain()) {
  // Whatever escapes is redacted too.
  process.on("uncaughtException", (err) => {
    process.stderr.write(`wakeshift-cli: ${redact(String(err?.stack ?? err))}\n`);
    process.exit(EXIT.FAIL);
  });
  process.on("unhandledRejection", (err) => {
    process.stderr.write(`wakeshift-cli: ${redact(String((err as Error)?.stack ?? err))}\n`);
    process.exit(EXIT.FAIL);
  });
  main(process.argv.slice(2), realCtx()).then((code) => {
    process.exitCode = code;
  });
}
