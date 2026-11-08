# Changelog

What changed in each release of `@wakeshift/cli`.

## 0.1.3 (2026-11-08)

- **`init` says plainly when you're on Wakeshift's waitlist**, and exits 3: "You're on Wakeshift's waitlist, so the app wasn't created. Wakeshift will email you when your account is ready; run init again then (it keeps the key in .env)." Wakeshift now answers the link's key with `403 waitlisted` once the waitlist page has seen the link, so `init` stops at once instead of waiting 10 minutes and timing out (earlier versions exit 3 there too, with Wakeshift's own message).

## 0.1.2 (2026-11-06)

- **`verify`'s cleanup report is accurate when it fails early.** It ends every subscription the tool made, by its ID from `list_subscriptions` (a tool that subscribes to several events, too), including after it fails at the event check, and passes once it sees none left. When it can't end them or check that it did, it says why, rather than warning that `unsubscribe_events` was missing when it was there.
- **`--trigger` can use values from the subscribing tool's reply**: `{result.<path>}` in the JSON its text starts with, `{structured.<path>}` in its `structuredContent`, such as `--trigger 'tool:cancel_build:{"number":"{result.number}"}'` for the build the tool started. In a `cmd:` trigger the value is quoted for the shell. A value that looks like a key, a session key or a stream link is refused.
- **`verify` ignores an occurrence that was waiting before its trigger**, such as an earlier run's that a subscription made with `replay` delivers at once, and says it did: only what arrives after the trigger passes the wake check.

## 0.1.1 (2026-11-06)

- **`verify`** subscribes as Claude Code (the tool's `harness` argument) and holds the stream as Claude Code's Monitor does: Wakeshift no longer gives a program it doesn't recognize a command to wait with, so the subscribe check failed against it. A tool whose reply has Wakeshift's block but no command still fails that check, and verify now unsubscribes after it.
- **`verify`'s cleanup passes when the subscription ended with the wake**, as one made with `once` does (the quickstart's `start_crawl`), rather than warning that it couldn't unsubscribe.
- **`verify`'s fixes name the SDKs' current API** (TypeScript `attached.registerTool` with `subscribes`, Go `wakeshift.AddTool`), from TypeScript 0.11.0 and Go v0.10.0 on.

## 0.1.0 (2026-11-04)

The first release.

- **`init`** makes an app key on your computer, writes it to `.env` (git-ignored, readable only by you), and creates the app through a console link you confirm, waiting until it exists.
- **`keys new`** adds a new key to an existing app the same way.
- **`status`** shows the app, its key and the events Wakeshift has from your server.
- **`verify`** acts as an agent against your running server: it calls the tool that subscribes, checks its description and reply, emits a test event (or runs your trigger) only when nobody else is subscribed with those arguments, waits for the wake, cleans up, and says what to fix.
- The key is never printed, never taken as an argument, and redacted from everything relayed.
