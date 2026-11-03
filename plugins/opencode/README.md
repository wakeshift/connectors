# @wakeshift/opencode

Wakeshift's plugin for [opencode](https://opencode.ai) (1.18.7 and later). Messages from the apps your agent watches through Wakeshift wake your opencode sessions as soon as they arrive, even while the session is idle, as long as opencode is open.

## Install

```sh
opencode plugin @wakeshift/opencode@<version> --global --force
```

opencode adds the plugin to the `plugin` list in `~/.config/opencode/opencode.json` and keeps your other settings. It downloads the plugin with Wakeshift's connector for your computer, one of `@wakeshift/connect-darwin-arm64`, `-darwin-x64`, `-linux-arm64` or `-linux-x64`. Then quit and restart opencode. `opencode --continue` reopens your session.

The plugin trusts https://app.wakeshift.ai. To trust another Wakeshift, give the plugin an `issuers` option in `opencode.json`:

```json
{ "plugin": [["@wakeshift/opencode@<version>", { "issuers": ["https://wakeshift.example.com"] }]] }
```

If you installed Wakeshift's plugin for opencode with `curl … | sh` before, this plugin removes that copy, `~/.config/opencode/plugins/wakeshift.ts`, the first time it loads.

## Update

Run the install command again with the new version, then restart opencode. `--force` replaces the old version and keeps the plugin's options.

## Remove

opencode has no command for removing a plugin:

1. Delete the `@wakeshift/opencode@…` entry from the `plugin` list in `~/.config/opencode/opencode.json`.
2. Delete `~/.wakeshift/opencode`, the plugin's settings and connected sessions.
3. Restart opencode.

## Source

Built and published from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors) with npm provenance.
