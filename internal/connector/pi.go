package connector

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// pi (earendil-works/pi, 1.5.0, verified) has no shell hooks; it has
// extensions, TypeScript modules it loads from ~/.pi/agent/extensions/
// ($PI_CODING_AGENT_DIR/extensions) with no trust step, again on /reload.
// An extension's tool_result handler gets an MCP tool's result, but pi's
// MCP client drops the result's _meta before any extension sees it (its
// mcp/tools.ts), so the beacon is a line ending the result's text (the
// Text carrier), which the extension takes out of what the model sees. The
// handler also gets the server and the bare MCP tool (details), so the
// server's pin and the beacon's tool are checked exactly. pi's MCP client
// tells servers nothing about the session (tools/call _meta has only
// progressToken), but the extension's tool_call handler sets the call's
// conversation_id to the session's ID (and harness to "pi") on the app's
// tools that declare them, so a text beacon is bound to the session, as
// Cursor's are by its agent: the core mints none without it (withBeacon).
//
// The extension (piExtension, written by the installer) runs one
// `wakeshift bridge --harness pi` per connected session, speaking opencode's
// JSON lines (Bridge), with the server and tool added to the first line:
//
//	{"hook_event_name": "PostToolUse" | "SessionIdle", "session_id", "call_id", "tool_name", "server", "mcp_tool", "tool_response": {"content": [...]}}
//
// It starts a run with what arrives only while pi is idle (never mid-turn):
// the messages go in the run's system prompt (a wakeshift_events section,
// which pi sends as a system message, never as the user), started by a
// fixed custom message; it acks them when that run starts.

const piName = "pi"

//go:embed pi_extension.ts
var piExtension string

const piMarker = "Wakeshift's extension for pi"

func init() {
	register(Spec{Name: piName, Format: piFormat{}, Installer: piInstaller{},
		When: "as soon as they arrive, even while the session is idle, or once its turn ends if it's busy, as long as pi is open", Unbound: true})
}

// piFormat reads the extension's lines; it answers as opencode's plugin is
// answered (additionalContext, wake).
type piFormat struct{ opencodeFormat }

// TextOnly: pi hands extensions an MCP result without its _meta, so a _meta
// in what the extension sends is text a tool returned, never the result's.
func (piFormat) TextOnly() bool { return true }

func (piFormat) Parse(b []byte) (Input, error) {
	var c struct {
		HookEventName string          `json:"hook_event_name"`
		SessionID     string          `json:"session_id"`
		CallID        string          `json:"call_id"`
		ToolName      string          `json:"tool_name"`
		Server        string          `json:"server"`
		MCPTool       string          `json:"mcp_tool"`
		ToolResponse  json.RawMessage `json:"tool_response"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Input{}, err
	}
	in := Input{Conversation: c.SessionID}
	switch c.HookEventName {
	case "PostToolUse":
		in.Event, in.ToolName, in.ToolCallID, in.ToolResult = ToolResult, c.ToolName, c.CallID, c.ToolResponse
		// The server and tool count only for the name pi gave that server's
		// tool: a result of anything else (a built-in tool, another
		// extension's) names no MCP tool, so no beacon in it is acted on.
		if c.Server != "" && c.MCPTool != "" && PiToolName(c.Server, c.MCPTool, c.ToolName) {
			in.Server, in.MCPTool = c.Server, c.MCPTool
		}
	case "SessionIdle":
		in.Event = Rewake
	}
	return in, nil
}

// piMaxTool is the longest tool name pi gives (providers take 64).
const piMaxTool = 64

// PiToolName reports whether name is the tool name pi gives server's MCP
// tool tool: mcp__<server>__<tool> with every character but ASCII letters,
// digits and _ turned into _ (one per UTF-16 unit, as pi's JavaScript
// does), or, when that's too long or taken by another tool, its first 55
// characters, _ and the first 8 hex digits of SHA-256(server NUL tool)
// (pi's createMcpToolName).
func PiToolName(server, tool, name string) bool {
	full := piSanitize("mcp__" + server + "__" + tool)
	if name == full && len(full) <= piMaxTool {
		return true
	}
	sum := sha256.Sum256([]byte(server + "\x00" + tool))
	hash := hex.EncodeToString(sum[:])[:8]
	short := full
	if len(short) > piMaxTool-len(hash)-1 {
		short = short[:piMaxTool-len(hash)-1]
	}
	return name == short+"_"+hash
}

func piSanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			for range max(1, utf16.RuneLen(r)) { // an invalid rune is one U+FFFD, one unit
				b.WriteByte('_')
			}
		}
	}
	return b.String()
}

// PiAgentDir is pi's agent directory: $PI_CODING_AGENT_DIR (a leading ~
// is the home folder), default ~/.pi/agent.
func PiAgentDir() string {
	home, _ := os.UserHomeDir()
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		if d == "~" {
			return home
		}
		if rest, ok := strings.CutPrefix(d, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return d
	}
	return filepath.Join(home, ".pi", "agent")
}

type piInstaller struct{}

func (piInstaller) path() string { return filepath.Join(PiAgentDir(), "extensions", "wakeshift.ts") }

// Install writes Wakeshift's own extension file (nothing of anyone else's is
// edited).
func (i piInstaller) Install(bin string, out io.Writer) error {
	return writeOwnFile(i.path(), []byte(PiExtension(bin)), out, "extension")
}

func (i piInstaller) Uninstall(bin string, out io.Writer) error {
	if err := removeOwnFile(i.path(), piMarker, out, "extension"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Type /reload in pi (or restart it) to unload it.")
	return nil
}

// PiExtension is the extension file, running bin.
func PiExtension(bin string) string {
	return strings.Replace(piExtension, "__WAKESHIFT_BIN__", jsString(bin), 1)
}

// piShared marks where the extension's own head ends and the part the npm
// package shares begins.
const piShared = "// ---- the rest is the npm package's too (internal/connector: PiPackage) ----\n"

// PiPackage is the extension as the npm package @wakeshift/pi ships it
// (plugins/pi/index.js, which TestPiPackage keeps in step: `go test
// ./internal/connector -run TestPiPackage -update` writes it). pi installs
// it with its own package command, `pi install npm:@wakeshift/pi@<version>`,
// which adds it to the packages list in ~/.pi/agent/settings.json and
// installs it with npm.
//
// It's the extension with its own head:
//
//   - The connector is this platform's optional dependency,
//     @wakeshift/connect-<os>-<arch>, the package @wakeshift/opencode and
//     npx -y @wakeshift/connect install it from too, made executable at
//     load.
//   - The Wakeshifts it trusts are DEFAULT_ISSUERS (production;
//     plugins/pack-npm.mjs --issuer packs it for another), added to
//     <home>/pi/config.json, where the bridge reads them, as `wakeshift
//     install` does. pi packages take no options.
//   - It removes the extension file an earlier `wakeshift install --harness
//     pi` wrote (piInstaller.path), so a user who moves over doesn't load
//     both from the next start; until then the body's guard lets only the
//     first copy act.
func PiPackage() string {
	_, body, ok := strings.Cut(piExtension, piShared)
	if !ok {
		panic("pi_extension.ts has no shared part")
	}
	return piPackageHead + piShared + body
}

// PiDefaultIssuers is the line of PiPackage that names the Wakeshifts it
// trusts by default (plugins/pack-npm.mjs replaces it to pack the package
// for another Wakeshift).
const PiDefaultIssuers = `const DEFAULT_ISSUERS = ["https://app.wakeshift.ai"]`

const piPackageHead = `// Wakeshift's extension for pi, the npm package @wakeshift/pi: lets Wakeshift
// messages reach your pi sessions, even while they're idle. Installed with
// ` + "`pi install npm:@wakeshift/pi@<version>`" + `; remove it with
// ` + "`pi remove npm:@wakeshift/pi`" + `, then type /reload in pi.
//
// Generated from the extension ` + "`wakeshift install --harness pi`" + ` writes
// (internal/connector, PiPackage), with three changes: the connector is this
// platform's package, @wakeshift/connect-<os>-<arch>; the Wakeshifts it trusts
// are DEFAULT_ISSUERS; and it removes the extension file an earlier install
// wrote.
import { spawn } from "node:child_process"
import { createInterface } from "node:readline"
import { createHash, randomBytes } from "node:crypto"
import { chmodSync, closeSync, existsSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync } from "node:fs"
import { createRequire } from "node:module"
import { homedir } from "node:os"
import { join } from "node:path"

` + PiDefaultIssuers + `
const ORIGIN = /^https?:\/\/[A-Za-z0-9.-]+(:[0-9]{1,5})?$/

// The connector for this platform, from the optional dependency npm picked,
// made executable here (a package's file modes aren't to be relied on).
const connector = () => {
  const bin = createRequire(import.meta.url).resolve(` + "`@wakeshift/connect-${process.platform}-${process.arch}/bin/wakeshift`" + `)
  if ((statSync(bin).mode & 0o111) === 0) chmodSync(bin, 0o755)
  return bin
}

// The Wakeshifts the connector trusts for pi, in the file the bridge reads
// (<home>/pi/config.json): DEFAULT_ISSUERS are added, nothing is removed.
// A file that's there but isn't a JSON object is left as it is (the bridge
// then trusts nothing from it, and says so in its log), never replaced.
const trust = (dir, issuers) => {
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const file = join(dir, "config.json")
  let cfg = {}
  if (existsSync(file)) {
    cfg = JSON.parse(readFileSync(file, "utf8")) // throws: ready() says no
    if (!cfg || typeof cfg !== "object" || Array.isArray(cfg)) throw new Error(file + " isn't a JSON object")
  }
  const have = Array.isArray(cfg.issuers) ? cfg.issuers : []
  const add = issuers.filter((i) => typeof i === "string" && ORIGIN.test(i) && !have.includes(i))
  if (!add.length && cfg.installed_at) return
  cfg.issuers = [...have, ...add]
  cfg.installed_at ??= new Date().toISOString()
  const tmp = file + "." + process.pid + "." + randomBytes(6).toString("hex") + ".tmp"
  try {
    writeFileSync(tmp, JSON.stringify(cfg, null, 2) + "\n", { mode: 0o600, flag: "wx" })
    renameSync(tmp, file)
  } catch (e) {
    try { unlinkSync(tmp) } catch {}
    throw e
  }
}

// The extension file ` + "`wakeshift install --harness pi`" + ` wrote, removed once
// this package runs instead (only a file that says it's that extension).
const retire = () => {
  const d = process.env.PI_CODING_AGENT_DIR
  const agent = !d ? join(homedir(), ".pi", "agent") : d === "~" ? homedir() : d.startsWith("~/") ? join(homedir(), d.slice(2)) : d
  const file = join(agent, "extensions", "wakeshift.ts")
  try {
    const src = readFileSync(file, "utf8")
    if (src.includes("` + piMarker + `") && src.includes("wakeshift install --harness pi")) unlinkSync(file)
  } catch {}
}

let BIN
// Sets up the connector and the Wakeshifts it trusts; false when there's no
// connector for this computer (or no home to write), and nothing to do.
const ready = () => {
  try {
    BIN = connector()
    trust(join(process.env.WAKESHIFT_HOME || join(homedir(), ".wakeshift"), "pi"), DEFAULT_ISSUERS)
  } catch {
    return false
  }
  retire()
  return true
}

`
