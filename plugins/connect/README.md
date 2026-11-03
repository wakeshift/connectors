# @wakeshift/connect

Installs Wakeshift's connector and its hooks for your agent's program, so messages from the apps your agent watches through Wakeshift reach its conversations, even while they're idle.

```sh
npx -y @wakeshift/connect <harness>
```

`<harness>` is one of `cursor`, `codex`, `claude-code`, `copilot`, `antigravity` or `opencode`. Run it when your agent offers it, or yourself. It needs Node 18 or later.

## What it does

- Copies Wakeshift's connector (`wakeshift`) for your computer to `~/.wakeshift/bin/wakeshift`. The binary comes from one of the platform packages (`@wakeshift/connect-darwin-arm64`, `-darwin-x64`, `-linux-arm64`, `-linux-x64`), and it's checked against the SHA-256 this package carries first.
- Runs `wakeshift install --harness <harness>`, which adds Wakeshift's hooks to your program's settings and keeps a backup. Your other settings are left alone.

Nothing runs when npm installs the package. Then restart your program, as the install says.

Every release is built and published from [github.com/wakeshift/connectors](https://github.com/wakeshift/connectors) with npm provenance.

## Options

- `--issuer <URL>`: trust a Wakeshift other than https://app.wakeshift.ai.
- `--uninstall`: remove the hooks again (`wakeshift uninstall --harness <harness>`).

## Update

Run the same command with `@latest` (`npx -y @wakeshift/connect@latest <harness>`), so npx doesn't reuse an older copy it cached.

## Exit codes

- 0: installed.
- 1: no platform package for this computer (npm skips them with `--omit=optional`), the registry couldn't be reached, or the install failed.
- 7: the binary didn't match its checksum. Nothing was installed.
- 64: an unknown harness or argument.
