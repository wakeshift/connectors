// Command wakeshift-connector is Wakeshift's connector on users' machines,
// installed as ~/.wakeshift/bin/wakeshift by /install/<harness>.sh: it
// lets Wakeshift messages wake their agents' conversations in local
// harnesses (Codex, Claude Code, Cursor, Copilot CLI, opencode, pi, Antigravity).
//
//	wakeshift install --harness NAME --issuer URL [--yes]   trust a Wakeshift and add the harness's hooks
//	wakeshift uninstall --harness NAME              remove them
//	wakeshift hook --harness NAME [--event E] [--rewake]   what the harness runs
//	wakeshift bridge --harness opencode             the opencode plugin's child
//	wakeshift bridge --harness pi                   the pi extension's child
//	wakeshift run --thread ID | status ID           Codex's per-thread connector
//	wakeshift grok init|prove --bot ID --issuer URL Grok Bot's key and proofs
//	wakeshift harnesses | version
//
// See internal/connectcli, internal/connector and internal/codexconnector.
package main

import "github.com/wakeshift/connectors/internal/connectcli"

func main() { connectcli.Main() }
