# @wakeshift/pi

Wakeshift's extension for [pi](https://github.com/earendil-works/pi), the terminal coding agent (1.5.0 and later). Messages from the apps your agent watches through Wakeshift wake your pi sessions as soon as they arrive, even while the session is idle, as long as pi is open. While pi is busy, they arrive once its turn ends.

## Install

```sh
pi install npm:@wakeshift/pi@<version>
```

pi adds the package to the `packages` list in `~/.pi/agent/settings.json` and keeps your other settings. npm downloads it with Wakeshift's connector for your computer, one of `@wakeshift/connect-darwin-arm64`, `-darwin-x64`, `-linux-arm64` or `-linux-x64`. Then type `/reload` in pi (or restart it with `pi --continue`). There's nothing to approve.

The extension trusts https://app.wakeshift.ai.

If you installed Wakeshift's extension for pi with `curl … | sh` before, this package removes that copy, `~/.pi/agent/extensions/wakeshift.ts`, the first time it loads.

## Update

Run the install command again with the new version, then type `/reload` in pi.

## Remove

```sh
pi remove npm:@wakeshift/pi
```

Then delete `~/.wakeshift/pi`, the extension's settings and connected sessions, and type `/reload` in pi.

## Source

Built and published from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors) with npm provenance.
