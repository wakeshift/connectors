# Changelog

What changed in each release of `@wakeshift/cli`.

## Unreleased

- **`verify`** subscribes as Claude Code (the tool's `harness` argument) and holds the stream as Claude Code's Monitor does: Wakeshift no longer gives a program it doesn't recognize a command to wait with, so the subscribe check failed against it. A tool whose reply has Wakeshift's block but no command still fails that check, and verify now unsubscribes after it.

## 0.1.0 (2026-11-04)

The first release.

- **`init`** makes an app key on your computer, writes it to `.env` (git-ignored, readable only by you), and creates the app through a console link you confirm, waiting until it exists.
- **`keys new`** adds a new key to an existing app the same way.
- **`status`** shows the app, its key and the events Wakeshift has from your server.
- **`verify`** acts as an agent against your running server: it calls the tool that subscribes, checks its description and reply, emits a test event (or runs your trigger) only when nobody else is subscribed with those arguments, waits for the wake, cleans up, and says what to fix.
- The key is never printed, never taken as an argument, and redacted from everything relayed.
