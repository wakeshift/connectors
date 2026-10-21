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
		When: s.When, StopWait: s.StopWait, RewakeWait: s.RewakeWait, Unbound: s.Unbound}
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
	if err := recordCreated(set, was); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wakeshift's hooks for %s run %s and trust beacons from %s.\n", s.Name, bin, issuer)
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
	// What the uninstall says goes out once removeCreated is done: a backup
	// it mentions may be deleted with the file it backed up (one the install
	// created), and then it isn't mentioned.
	var said, after bytes.Buffer
	err := s.Installer.Uninstall(bin, &said)
	if err == nil && filed {
		for _, b := range removeCreated(set, s.Installer, newSince(before, backupsOf(f.path())), &after) {
			said = *bytes.NewBufferString(strings.ReplaceAll(said.String(), " (the previous file is at "+b+")", ""))
		}
	}
	out.Write(said.Bytes())
	out.Write(after.Bytes())
	if err != nil {
		return err
	}
	// Its trust and state here too (its logs stay, for the user).
	dir := set.Dir
	for _, f := range []string{"config.json", "keys", "links", "waiters", "notes", "pending", pinsFile, createdFile} {
		os.RemoveAll(filepath.Join(dir, f))
	}
	return nil
}
