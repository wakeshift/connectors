package connector

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Spec is how Wakeshift connects to one harness on this machine.
type Spec struct {
	Name      string // as in --harness, and internal/harnesses
	Format    Format
	Installer Installer
	// When, for the "connected" note: when messages reach the conversation.
	When string
	// StopWait: how long the stop hook listens (under the harness's hook
	// timeout, which the installer sets).
	StopWait time.Duration
	// RewakeWait: how long an async rewake hook listens (0: until the
	// harness stops it).
	RewakeWait time.Duration
	// Unbound: its MCP clients tell servers neither the conversation nor
	// the call, so its hook accepts beacons naming neither (beacon.Binding).
	Unbound bool
	// Wait: its agent is woken by a command it starts in the background,
	// so its hooks tell Wakeshift the wait command (Hook.WaitCommand).
	Wait bool
	// RecordProcs: its commands aren't told their conversation, so its
	// hooks record the harness process for `wakeshift wait`.
	RecordProcs bool
	// Rule, if set, is the exact allowlist rule for the wait command that
	// installing adds, and where (rules.go).
	Rule func(waitCommand string) AllowRule
	// Tools, if set, is how the harness pre-approves an app's MCP tools
	// with the user's yes (toolrules.go): only harnesses with exact
	// per-tool rules.
	Tools *ToolRules
}

var specs = map[string]Spec{}

func register(s Spec) { specs[s.Name] = s }

// Lookup returns a harness's spec.
func Lookup(name string) (Spec, bool) {
	s, ok := specs[name]
	return s, ok
}

// Names lists the harnesses with a spec here.
func Names() []string {
	var n []string
	for k := range specs {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// NewHook is a spec's hook with this machine's settings.
func (s Spec) NewHook(hc *http.Client, p Poller) *Hook {
	set := LoadSettings(s.Name)
	if p == nil {
		p = &HTTPPoller{Harness: s.Name}
	}
	return &Hook{Harness: s.Name, Format: s.Format, Verifier: &Verifier{Settings: set, HTTP: hc}, Poller: p, Links: LinksFor(set), HTTP: hc,
		When: s.When, StopWait: s.StopWait, RewakeWait: s.RewakeWait, Unbound: s.Unbound, RecordProcs: s.RecordProcs, Tools: s.Tools}
}

// Install trusts issuer to sign beacons for this harness and adds its
// hooks, saying what it changed (the caller then says the harness's trust
// step).
func (s Spec) Install(bin, issuer string, out io.Writer) error {
	issuer, err := CheckIssuer(issuer)
	if err != nil {
		return err
	}
	set := LoadSettings(s.Name)
	if err := set.Trust(issuer); err != nil {
		return err
	}
	var was []string
	if f, ok := s.Installer.(hookFiler); ok {
		was = missingPaths(f.path())
	}
	if err := s.Installer.Install(bin, out); err != nil {
		return err
	}
	set.InstalledAt = time.Now().UTC()
	if err := set.Save(); err != nil {
		return err
	}
	if err := recordCreated(set, was); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wakeshift's hooks for %s run %s and trust beacons from %s.\n", s.Name, bin, issuer)
	addRule(set, s, bin, out, was)
	return nil
}

// Uninstall removes the harness's hooks, the issuers it trusted and its
// conversations' links, so nothing here listens for it any more.
func (s Spec) Uninstall(bin string, out io.Writer) error {
	set := LoadSettings(s.Name)
	var before []string
	f, filed := s.Installer.(hookFiler)
	if filed {
		before = backupsOf(f.path())
	}
	// The files the install added rules to, other than its hooks file.
	rules := map[string][]string{}
	for _, p := range rulePaths(set) {
		if !filed || filepath.Clean(p) != filepath.Clean(f.path()) {
			rules[p] = backupsOf(p)
		}
	}
	// What the uninstall says goes out once removeCreated is done: a backup
	// it mentions may be deleted with the file it backed up (one the install
	// created), and then it isn't mentioned.
	var said, after bytes.Buffer
	err := s.Installer.Uninstall(bin, &said)
	if err == nil {
		removeRules(set, &said)
		var gone []string
		if filed {
			gone = removeCreated(set, s.Installer, newSince(before, backupsOf(f.path())), &after)
		}
		for p, was := range rules {
			gone = append(gone, removeCreatedFile(set, p, emptyJSON, newSince(was, backupsOf(p)), &after)...)
		}
		for _, b := range gone {
			said = *bytes.NewBufferString(strings.ReplaceAll(said.String(), " (the previous file is at "+b+")", ""))
		}
	}
	out.Write(said.Bytes())
	out.Write(after.Bytes())
	if err != nil {
		return err
	}
	// Its directory here too (<home>/<harness>: trust, links, keys, state and
	// logs), so nothing of it is left. It's this harness's alone, whichever
	// apps and Wakeshifts it connected: the hooks it served are gone for all
	// of them. A list of what to remove fell behind what hooks write there
	// (S6 found it left on October 28), so the whole directory goes, and a
	// hook that still runs in an open session doesn't make it again
	// (Settings.Installed).
	if dir := set.Dir; ownDir(dir, s.Name) {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("the hooks are removed, but not %s (its trust and links): %w", dir, err)
		}
	}
	return nil
}

// ownDir: dir is a registered harness's directory under Wakeshift's home
// (never bin/, procs/ or another's), and an absolute path (so a missing
// home never makes it one relative to here).
func ownDir(dir, harness string) bool {
	home := Home()
	_, known := Lookup(harness)
	return known && filepath.Base(harness) == harness && filepath.IsAbs(home) &&
		filepath.Clean(dir) == filepath.Join(home, harness)
}
