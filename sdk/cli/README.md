# @wakeshift/cli

`wakeshift-cli` sets up a [Wakeshift](https://wakeshift.ai) app for your MCP server and checks that the server wakes agents. Your coding agent can run it for you, and it never shows the app's key to anyone: not on the screen, not in a link, not in the agent's transcript.

```sh
npx -y @wakeshift/cli@0.1.3 init
npx -y @wakeshift/cli@0.1.3 verify --url http://localhost:3000/mcp --tool watch_build --args '{"number":412}'
```

It needs Node 22 or later, and has no dependencies beyond Node itself.

## Commands

### init

```sh
npx -y @wakeshift/cli init [--name <id>] [--display-name <name>] [--env <path>] [--replace]
```

1. Makes a new app key on your computer, from the operating system's secure random source.
2. Writes it to `.env` as `WAKESHIFT_KEY` (a new file is readable only by you). If `.env` isn't ignored by git, it adds it to `.gitignore`; if git tracks `.env`, it stops.
3. Prints a link to the Wakeshift console and a key code, and opens the link in your browser when there's one to open. Open it, sign in and check that the page shows the same key code, then press **Create app**. The link carries only the key's public half, after the `#`, so it never reaches a server log.
4. Waits until the app exists (up to 10 minutes), then prints `Created <id>. WAKESHIFT_KEY is in .env.`

The app is named `<project>-dev` (from `package.json`, `pyproject.toml`, `go.mod` or the directory), since you test with a separate app from the one your users get; you can rename it on the page. If `init` stops before the app exists, run it again: it keeps the key in `.env` and waits again. `--replace` makes a new key in place of one that's already there.

### keys new

```sh
npx -y @wakeshift/cli keys new [--app <id>] [--env <path>]
```

A new key for an existing app, the same way: made here, added through a console link you confirm. Until you confirm it, it waits in `.env` as `WAKESHIFT_KEY_NEW`; then it becomes `WAKESHIFT_KEY`. The old key keeps working until you revoke it in the console.

### status

```sh
npx -y @wakeshift/cli status [--json]
```

The app and key in `.env`, and whether Wakeshift has your server's events yet (your server sends them when it attaches and handles its first call about events).

### verify

```sh
npx -y @wakeshift/cli verify --url <mcp url> --tool <name> [--args <json>] [--trigger tool:<name>[:<json>] | --trigger cmd:<command>] [--header-env <NAME>] [--json]
```

With your server running (and `.env` loaded), `verify` acts as an agent (Claude Code, as the tool's `harness` argument says, holding the stream as its Monitor does):

| Check | Passes when |
| --- | --- |
| app | the key in `.env` is an app's |
| live app | the app's ID ends in `-dev` or `-test`, or you pass `--live-app` |
| tool | the server lists the tool, with Wakeshift's inputs (`session_key`, `harness`, `conversation_id`) |
| description | the tool's description says it notifies (the sentence `subscribes` adds) |
| subscribe | calling the tool with `--args` subscribes the conversation: the reply carries Wakeshift's block |
| event | Wakeshift has the event the tool declares |
| arguments | verify can tell the arguments the tool subscribed with (from `list_subscriptions`, `--event-args` or `--args`) |
| guard | nobody but verify is subscribed with those arguments |
| trigger | the event is made to happen: `--trigger tool:<name>[:<json>]` calls one of your tools, `--trigger cmd:<command>` runs a command (a request to your webhook route), and without `--trigger` verify emits a test event itself |
| wake | the event reaches verify's conversation within 30 seconds (`--timeout`) of the trigger. Anything already waiting before the trigger, such as an earlier occurrence a subscription made with `replay` delivers at once, doesn't count, and verify says it ignored it |
| cleanup | verify's subscriptions are gone: they ended with the wake (a tool that subscribes with `once`), or verify ended them through `unsubscribe_events` (every one the tool made, by its ID from `list_subscriptions`) or an `un<tool>` next to the tool. When it can't end or check them, it says why |

Each failure comes with what to fix. `verify` exits 0 only when nothing failed.

**It never wakes your users.** Before anything is emitted, it asks Wakeshift who is subscribed with those arguments; if anyone besides verify is, it stops without emitting or running the trigger. Use routing values only verify uses: put `wsverify-` and a random suffix in a free-text argument, such as `--args '{"pipeline":"wsverify-3f9a"}'`. It runs against a development app unless you pass `--live-app`, and it unsubscribes when it's done.

**Values from the tool's reply in the trigger.** When the event needs something the subscribing tool returns, such as the number of the build it started, name it in `--trigger`: `{result.<path>}` is a value in the JSON the tool's text reply starts with, and `{structured.<path>}` one in its `structuredContent`. A path is keys and array indexes separated by dots (`{result.build.number}`, `{structured.items.0.id}`).

```sh
npx -y @wakeshift/cli verify --url http://localhost:3000/mcp --tool start_build --args '{"branch":"wsverify-3f9a"}' \
  --trigger 'tool:cancel_build:{"number":"{result.number}"}'
```

In a tool trigger's JSON, a string that is only a placeholder becomes the value itself (a number stays a number); anywhere else the value is written as text, and in a `cmd:` trigger it's quoted for the shell (`cmd:curl -X POST http://localhost:3000/builds/{result.number}/cancel`). verify refuses a value that looks like a key, a session key or a stream link, and redacts the trigger's output as before.

**A server with its own sign-in.** Pass `--header-env NAME`, the name of an environment variable that holds a test user's token: verify sends it as `Authorization: Bearer <token>`. The token never goes on the command line, and verify never prints it.

Other options: `--event <name>` when the tool declares several events, `--event-args <json>` when verify can't tell the arguments, `--user <id>` (the test user's ID, for a personal event), `--payload <json>` for the test event's payload.

## Options for every command

| Option | |
| --- | --- |
| `--env <path>` | the env file (default `.env`) |
| `--origin <url>` | another Wakeshift (default `https://app.wakeshift.ai`, or `WAKESHIFT_CORE`). `init` writes `WAKESHIFT_CORE` to `.env` when it isn't the default, so the SDK uses it too |
| `--no-browser` | print the link without opening it (`init`, `keys new`). `WAKESHIFT_NO_BROWSER=1` does the same; `$BROWSER` picks the browser |
| `--wait <seconds>` | how long `init` and `keys new` wait for you to confirm (default 600) |

## Exit codes

| Code | Means |
| --- | --- |
| 0 | done; for `verify`, every check passed |
| 1 | failed: the message says what to fix |
| 2 | the command line was wrong |
| 3 | the person who opened `init`'s link is on Wakeshift's waitlist: Wakeshift emails them when their account is ready; run `init` again then |
| 4 | timed out waiting for the console: run the same command again to keep waiting |

## Keeping the key secret

- The key is made on your computer and written straight to `.env`. Wakeshift only ever receives its public half, and every request is signed with a short-lived assertion instead.
- `wakeshift-cli` never prints the key, never takes one as an argument (it refuses a command line that has one), and removes anything key-shaped, stream links and tokens from every message it relays from your server, a trigger command or Wakeshift.
- Don't read or print `.env` to check the key: `status` and `verify` do it without showing it. With Claude Code, you can add `Read(.env)` to your deny rules.

## Troubleshooting

- **`init` exited 3.** You're on Wakeshift's waitlist. When the email says your account is ready, run `init` again: it keeps the key and prints the link again.
- **`init` timed out.** Open the link again (run the same command: it prints it again with the same key). If the page said your account is on the waitlist, Wakeshift emails you when it's ready.
- **verify: "can't tell the arguments".** Keep `attach`'s default tools on (`list_subscriptions` lets verify see its exact subscription), or pass `--event-args`.

## License

Apache License 2.0; see [LICENSE](LICENSE). Source: [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors), `sdk/cli`.
