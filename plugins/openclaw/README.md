# Wakeshift for OpenClaw

Wakeshift's plugin for OpenClaw (2026.10.4 and later). It wakes an OpenClaw session when an app the session watches through Wakeshift sends something, even while the session is idle, as long as the gateway runs. Messages arrive as outside information, never as the user.

## Install and remove

Install it where the OpenClaw gateway runs (in its container, if it's in one), the way the app's watch result says: `curl -fsSL https://app.wakeshift.ai/install/openclaw.sh | sh`. It checks the download's SHA-256, sets the Wakeshift the plugin trusts, and installs it with `openclaw plugins install`.

Remove it, and undo what it changed, with `curl -fsSL https://app.wakeshift.ai/install/openclaw-uninstall.sh | sh`.

## How it wakes a session

The plugin starts a turn with OpenClaw's own hooks endpoint, `POST <hooks.path>/agent` on the gateway's listener, in the session's own context (`sessionMode: "persistent"`). OpenClaw wraps the message as outside content. The turn's reply goes to the channel the session last spoke on, or stays in its transcript when it has none. A message is acknowledged to Wakeshift once the turn it started has finished.

When it first connects a session, the plugin turns on OpenClaw's hooks:

- `hooks.enabled`, with a random token of its own in `hooks.token`;
- `hooks.allowRequestSessionKey`, with `hooks.defaultSessionKey: "hook:wakeshift"`;
- `hooks.allowedSessionKeyPrefixes` and `hooks.allowedAgentIds`, allowing only the sessions and agents that connect through Wakeshift, each removed again when its session's subscription ends.

If OpenClaw's hooks are on already, the plugin only adds the session and its agent, and only if your settings already let a caller name the session from a restricted prefix list; otherwise it changes nothing and tells the agent what it would need. Everything it changes is recorded in `~/.openclaw/wakeshift/hooks.json`, which the uninstaller uses to put your settings back.

The hooks endpoint shares the gateway's listener. On a gateway that listens beyond this computer (its bind isn't `loopback`), the endpoint is reachable there too, guarded by the token, so the installer stops and asks first. To agree, run it with `WAKESHIFT_OPENCLAW_NETWORK_HOOKS=1`, and block `/hooks/` from other machines with a firewall or proxy rule.

## Settings

- `plugins.entries.wakeshift.config.issuer`: the Wakeshift whose beacons the plugin trusts (the installer sets it).
- `plugins.entries.wakeshift.config.allowNetworkHooks`: you agreed to the hooks endpoint on a gateway that listens beyond this computer (the installer sets it).

## Development

`npm test` runs the tests; `npm run pack` builds `wakeshift-openclaw.tgz`. The design and what was verified are in `docs/harnesses.md` (OpenClaw) in Wakeshift's main repository.
