package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/version"
	"github.com/wakeshift/connectors/plugins/hermes"
)

// Hermes Agent (0.22.2, verified) loads Python plugins
// from $HERMES_HOME/plugins/<name>/ once they're enabled (plugins.enabled).
// Its MCP client tells servers neither the session nor the call, so beacons
// are unbound; it hands an MCP result (with _meta, which its model sees) to
// a plugin's transform_tool_result hook with the session's ID. Wakeshift's
// plugin (plugins/hermes) does the rest itself in Python: it takes the
// beacon out, redeems it, holds the conversation's stream (SSE, else long
// poll) and starts a run in the same session once it's idle (the API
// server's /v1/runs, or the CLI's input queue), acknowledging each message
// once Hermes accepts it. So there's no hook command: install writes the
// plugin, pins the issuer in its config.json and enables it.

const hermesName = "hermes"

func init() {
	register(Spec{Name: hermesName, Format: hermesFormat{}, Installer: hermesInstaller{},
		When: "as soon as they arrive, even while it's idle, as long as Hermes is running", Unbound: true})
}

// hermesFormat: the plugin runs no hook command; every call is ignored.
type hermesFormat struct{}

func (hermesFormat) Parse([]byte) (Input, error) {
	return Input{}, errors.New("hermes runs no hook command")
}
func (hermesFormat) Context(Input, string) (Output, bool)  { return Output{}, false }
func (hermesFormat) Continue(Input, string) (Output, bool) { return Output{}, false }

// HermesHome is Hermes's home ($HERMES_HOME, default ~/.hermes).
func HermesHome() string {
	if d := os.Getenv("HERMES_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hermes")
}

// hermesBins are where Hermes puts its command when it isn't on PATH (its
// own terminal tool doesn't always have it): $HERMES_BIN, its Docker image
// (/opt/hermes/bin, and its venv), its installer's venv, pip's --user bin.
func hermesBins() []string {
	home, _ := os.UserHomeDir()
	return []string{os.Getenv("HERMES_BIN"), "/opt/hermes/bin/hermes", "/opt/hermes/.venv/bin/hermes", filepath.Join(HermesHome(), "hermes-agent", "venv", "bin", "hermes"),
		filepath.Join(home, ".local", "bin", "hermes")}
}

// HermesCommand runs `hermes args…` (it's how a plugin is enabled); var for
// tests. It fails if hermes is neither on PATH nor in hermesBins.
var HermesCommand = func(out io.Writer, args ...string) error {
	bin, err := exec.LookPath("hermes")
	for _, c := range hermesBins() {
		if err == nil {
			break
		}
		if st, serr := os.Stat(c); c != "" && serr == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			bin, err = c, nil
		}
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

type hermesInstaller struct{}

func (hermesInstaller) dir() string { return filepath.Join(HermesHome(), "plugins", "wakeshift") }

// Install writes the plugin (refusing a "wakeshift" plugin that isn't
// Wakeshift's), its config.json with the issuers this machine trusts for
// Hermes, and enables it with Hermes's own command.
func (i hermesInstaller) Install(bin string, out io.Writer) error {
	dir := i.dir()
	if b, err := os.ReadFile(filepath.Join(dir, "__init__.py")); err == nil && !strings.Contains(string(b), hermes.Marker) {
		return fmt.Errorf("%s isn't Wakeshift's plugin; left it alone", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	err := fs.WalkDir(hermes.Files, "wakeshift", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := hermes.Files.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, path.Base(p)), b, 0o644)
	})
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, hermes.VersionFileName), hermes.VersionFile(version.Version), 0o644)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote Wakeshift's plugin for Hermes to %s.\n", dir)
	if err := hermesConfig(filepath.Join(dir, "config.json"), LoadSettings(hermesName).Issuers, out); err != nil {
		return err
	}
	if err := HermesCommand(out, "plugins", "enable", "wakeshift"); err != nil {
		fmt.Fprintln(out, "Couldn't enable it here: run `hermes plugins enable wakeshift` where Hermes runs (in Hermes's Docker image: /opt/hermes/bin/hermes plugins enable wakeshift).")
		return nil
	}
	fmt.Fprintln(out, "Enabled it in Hermes (hermes plugins enable wakeshift).")
	return nil
}

// hermesConfig writes the plugin's config.json, or on a reinstall keeps the
// one there: its values stay as they are, keys it lacks are added, and an
// issuer trusted now that it doesn't list is added to its issuers. It says
// what changed.
func hermesConfig(path string, issuers []string, out io.Writer) error {
	cfg := map[string]any{}
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if json.Unmarshal(old, &cfg) != nil || cfg == nil {
			return fmt.Errorf("%s isn't JSON; fix or move it, then run this again", path)
		}
	}
	var added []string
	have, _ := cfg["issuers"].([]any)
	if _, ok := cfg["issuers"]; ok && have == nil {
		return fmt.Errorf("%s: issuers isn't a list; fix or move it, then run this again", path)
	}
	for _, iss := range issuers {
		found := false
		for _, h := range have {
			if h == iss {
				found = true
			}
		}
		if !found {
			have = append(have, iss)
			added = append(added, iss)
		}
	}
	if have == nil {
		have = []any{}
	}
	cfg["issuers"] = have
	switch {
	case err == nil && len(added) == 0:
		fmt.Fprintf(out, "Kept its settings, %s, as they were.\n", path)
		return nil
	case err == nil:
		fmt.Fprintf(out, "Kept its settings, %s, and added %s to the Wakeshifts it trusts.\n", path, strings.Join(added, ", "))
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Uninstall disables the plugin, removes it if it's Wakeshift's, and removes
// its state (the conversations' links, the keys it cached).
func (i hermesInstaller) Uninstall(bin string, out io.Writer) error {
	dir := i.dir()
	b, err := os.ReadFile(filepath.Join(dir, "__init__.py"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(out, "Wakeshift's plugin for Hermes wasn't at %s.\n", dir)
	case err != nil:
		return err
	case !strings.Contains(string(b), hermes.Marker):
		return fmt.Errorf("%s isn't Wakeshift's plugin; left it alone", dir)
	default:
		if err := HermesCommand(io.Discard, "plugins", "disable", "wakeshift"); err == nil {
			fmt.Fprintln(out, "Disabled it in Hermes (hermes plugins disable wakeshift).")
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		// Its state (links, keys, what started), only with the plugin that
		// kept it: a HERMES_HOME without it may be another Hermes's.
		os.RemoveAll(filepath.Join(HermesHome(), "wakeshift"))
		fmt.Fprintf(out, "Removed Wakeshift's plugin for Hermes, %s, and its state.\n", dir)
	}
	return nil
}
