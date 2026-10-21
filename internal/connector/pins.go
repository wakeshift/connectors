package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Pins keep which app's beacons each MCP server's results may carry, for
// harnesses whose hooks say which server a result came from. A beacon's
// signature says Wakeshift minted it, not that it came from the app it
// names: any MCP server (or text a tool returns) could carry one an
// attacker minted for this conversation through some app. So a server's
// results carry one app's beacons only, and an app's beacons come from one
// server only: the first beacon from a server, for an app no other server
// has carried, pins the pair (servers.json, 0600). After that a beacon for
// that app from another server, or another app's from that server, is
// refused. If the user renames or re-adds the server, they remove its
// entry from the file.
type Pins struct {
	Path string // <home>/<harness>/servers.json
}

// PinsFor is a harness's pins.
func PinsFor(s Settings) Pins { return Pins{Path: filepath.Join(s.Dir, pinsFile)} }

const pinsFile = "servers.json"

type pinFile struct {
	Servers map[string]string `json:"servers"` // MCP server name → app ID
}

// ErrPinned: the server carries another app's beacons, or the app's come
// from another server.
type ErrPinned struct {
	Server, App  string // what this result says
	PinnedServer string // the server the app's beacons come from ("" if none)
	PinnedApp    string // the app the server's beacons are for ("" if none)
}

func (e *ErrPinned) Error() string {
	if e.PinnedApp != "" && e.PinnedApp != e.App {
		return fmt.Sprintf("the MCP server %q carries beacons for %q on this computer, not %q", e.Server, e.PinnedApp, e.App)
	}
	return fmt.Sprintf("beacons for %q come from the MCP server %q on this computer, not %q", e.App, e.PinnedServer, e.Server)
}

// Allow checks that server may carry app's beacons, pinning the pair the
// first time either is seen.
func (p Pins) Allow(server, app string) error {
	if server == "" || app == "" {
		return errors.New("connector: pinning needs a server and an app")
	}
	if !filepath.IsAbs(p.Path) {
		return errors.New("connector: the pins need an absolute path")
	}
	// Hooks run as separate processes: hold the pins' lock across the
	// read, the check and the write, so two first beacons can't both pin.
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(p.Path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	f, err := p.load()
	if err != nil {
		return err
	}
	if got, ok := f.Servers[server]; ok {
		if got == app {
			return nil
		}
		return &ErrPinned{Server: server, App: app, PinnedApp: got}
	}
	for s, a := range f.Servers {
		if a == app {
			return &ErrPinned{Server: server, App: app, PinnedServer: s}
		}
	}
	f.Servers[server] = app
	return p.save(f)
}

// ErrPinFile: the pins exist but can't be read. Pinning fails closed: read
// as none, the next beacon would pin over what the user had.
type ErrPinFile struct {
	Path string
	Err  error
}

func (e *ErrPinFile) Error() string { return fmt.Sprintf("can't read %s: %v", e.Path, e.Err) }
func (e *ErrPinFile) Unwrap() error { return e.Err }

func (p Pins) load() (pinFile, error) {
	f := pinFile{Servers: map[string]string{}}
	b, err := os.ReadFile(p.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil // none yet
	}
	if err != nil {
		return f, &ErrPinFile{Path: p.Path, Err: err}
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, &ErrPinFile{Path: p.Path, Err: err}
	}
	if f.Servers == nil {
		f.Servers = map[string]string{}
	}
	return f, nil
}

func (p Pins) save(f pinFile) error {
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(p.Path), "."+pinsFile+".*.tmp") // 0600
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p.Path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// MCPServerOf is the MCP server a tool name like mcp__<server>__<tool>
// (Codex's, Claude Code's, Hermes's) names, "" for any other name.
func MCPServerOf(tool string) string {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

// noRedirects is the hooks' http.Client CheckRedirect: a redeem's ticket
// and a poll's link token go only where the beacon or link says (the
// issuer's origin), never where a 3xx points.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// NotePinned is the note for a beacon refused by the pins.
const NotePinned = "Wakeshift: didn't connect this conversation: %s. If that's the server you use for this app (you renamed or re-added it), remove its line from %s and call the app's tool again."

// NotePinFile is the note when the pins can't be read.
const NotePinFile = "Wakeshift: didn't connect this conversation: %s can't be read, so Wakeshift can't check which MCP server carries this app's messages. Fix the file, or remove it to start over, and call the app's tool again."
