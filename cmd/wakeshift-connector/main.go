// Command wakeshift-connector is Wakeshift's connector on users' machines,
// installed as ~/.wakeshift/bin/wakeshift by /install/<harness>.sh: it
// lets Wakeshift messages wake their agents' conversations in local
// harnesses (Codex, Claude Code, Cursor, Copilot CLI, opencode, Antigravity).
//
//	wakeshift install --harness NAME --issuer URL   trust a Wakeshift and add the harness's hooks
//	wakeshift uninstall --harness NAME              remove them
//	wakeshift hook --harness NAME [--event E] [--rewake]   what the harness runs
//	wakeshift bridge --harness opencode             the opencode plugin's child
//	wakeshift run --thread ID | status ID           Codex's per-thread connector
//	wakeshift harnesses | version
//
// See internal/connectcli, internal/connector, internal/codexconnector and
// docs/harnesses.md.
package main

import "github.com/clawvisor/wakeshift-plugins/internal/connectcli"

func main() { connectcli.Main() }
