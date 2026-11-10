package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/connectors/internal/framing"
)

// Links keeps each conversation's stream links (secrets: 0600 files in
// 0700 directories), one per app the conversation is connected to, so
// every app it watches wakes it (docs/harnesses.md, "Several apps in one
// conversation"). Named by hashes of the conversation ID and the app, so
// odd IDs can't escape the directory:
//
//	links/<hash(conversation)>/<hash(app)>.json   a link per app
//	links/<hash(conversation)>.json               one saved before links were
//	                                              kept per app (or naming none)
//
// A conversation's links are only ever read by its own hooks, each handed
// to its own Wakeshift session's stream; one app's link is never sent with
// another app's redeem.
type Links struct {
	Dir string // <home>/<harness>/links
}

// LinksFor is a harness's link store.
func LinksFor(s Settings) Links { return Links{Dir: filepath.Join(s.Dir, "links")} }

type savedLink struct {
	Conversation string    `json:"conversation"`
	Link         string    `json:"link"`
	App          string    `json:"app,omitempty"` // the app whose beacon connected it
	SavedAt      time.Time `json:"saved_at"`
}

// Link is one of a conversation's stream links, and the app whose beacon
// connected it ("" for a link saved before links named their app).
type Link struct {
	App  string
	Link string
}

func hashName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// path is the conversation's own file: its link from before links were kept
// per app, and the name every other file of the conversation's is keyed by
// (notes, deferred results, wait locks).
func (l Links) path(conversation string) string {
	return filepath.Join(l.Dir, hashName(conversation)+".json")
}

// appDir holds the conversation's links per app.
func (l Links) appDir(conversation string) string {
	return filepath.Join(l.Dir, hashName(conversation))
}

func (l Links) appPath(conversation, app string) string {
	return filepath.Join(l.appDir(conversation), hashName(app)+".json")
}

// Save keeps a link for the conversation where there's no app to name (a
// test's, or one from before links named their app); a link that rotated
// goes through Rotate.
func (l Links) Save(conversation, link string) error {
	return l.SaveFor(conversation, link, "")
}

// SaveFor keeps the conversation's link for app's beacon. It replaces only
// app's own link (and one saved before links were kept per app, which was
// this app's or named none); another app's stays.
func (l Links) SaveFor(conversation, link, app string) error {
	if conversation == "" || link == "" {
		return errors.New("connector: saving an empty link")
	}
	if app == "" {
		return l.write(l.path(conversation), savedLink{Conversation: conversation, Link: link})
	}
	if err := os.MkdirAll(l.appDir(conversation), 0o700); err != nil {
		return err
	}
	if err := l.write(l.appPath(conversation, app), savedLink{Conversation: conversation, Link: link, App: app}); err != nil {
		return err
	}
	if old, ok := l.legacy(conversation); ok && (old.App == "" || old.App == app) {
		os.Remove(l.path(conversation))
	}
	return nil
}

func (l Links) write(p string, s savedLink) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	s.SavedAt = time.Now().UTC()
	b, _ := json.Marshal(s)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// All is every link the conversation has, one per app: the one saved
// before links were kept per app first, then the others by app.
func (l Links) All(conversation string) []Link {
	if conversation == "" {
		return nil
	}
	var out []Link
	if s, ok := l.legacy(conversation); ok {
		out = append(out, Link{App: s.App, Link: s.Link})
	}
	entries, _ := os.ReadDir(l.appDir(conversation))
	var apps []Link
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, ok := l.read(filepath.Join(l.appDir(conversation), e.Name()), conversation)
		// Only a file named for what it holds.
		if !ok || s.App == "" || hashName(s.App)+".json" != e.Name() {
			continue
		}
		apps = append(apps, Link{App: s.App, Link: s.Link})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].App < apps[j].App })
	return append(out, apps...)
}

// Load is one of the conversation's links ("" without one): where any will
// do (whether it's connected at all).
func (l Links) Load(conversation string) string {
	if all := l.All(conversation); len(all) > 0 {
		return all[0].Link
	}
	return ""
}

// LoadFor is the conversation's link for app ("" without one).
func (l Links) LoadFor(conversation, app string) string {
	for _, k := range l.All(conversation) {
		if k.App == app {
			return k.Link
		}
	}
	return ""
}

// Apps are the apps whose beacons connected the conversation ("" for a link
// that names none).
func (l Links) Apps(conversation string) []string {
	var out []string
	for _, k := range l.All(conversation) {
		out = append(out, k.App)
	}
	return out
}

// Rotate keeps next in place of the conversation's link old (a rotation),
// for the same app; nothing if the conversation no longer has old.
func (l Links) Rotate(conversation, old, next string) error {
	for _, k := range l.All(conversation) {
		if k.Link == old {
			return l.SaveFor(conversation, next, k.App)
		}
	}
	return nil
}

func (l Links) legacy(conversation string) (savedLink, bool) {
	return l.read(l.path(conversation), conversation)
}

func (l Links) read(p, conversation string) (savedLink, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return savedLink{}, false
	}
	var s savedLink
	if json.Unmarshal(b, &s) != nil || s.Conversation != conversation || s.Link == "" {
		return savedLink{}, false
	}
	return s, true
}

// waitLocks is where each running `wakeshift wait` holds a file of its own
// locked for its conversation (<harness dir>/waits/<hash>.<pid>.lock): the
// lock ends with the process, however it ends, so the hooks can tell
// whether one runs.
func (l Links) waitLocks(conversation string) (dir, prefix string) {
	return filepath.Join(filepath.Dir(l.Dir), "waits"), filepath.Base(l.path(conversation)) + "."
}

// HoldWait marks a wait command running for the conversation until the
// returned func (or the process's end) releases it.
func (l Links) HoldWait(conversation string) func() {
	dir, prefix := l.waitLocks(conversation)
	// Mkdir, not MkdirAll: uninstalled meanwhile, the harness's directory
	// is gone, and this doesn't make it again.
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return func() {}
	}
	p := filepath.Join(dir, prefix+strconv.Itoa(os.Getpid())+".lock")
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return func() {}
	}
	return func() {
		os.Remove(p)
		f.Close()
	}
}

// WaitRunning says whether a wait command holds a lock for the
// conversation now: a process that ended (killed, or its harness
// restarted) doesn't. Files left by ended ones are removed.
func (l Links) WaitRunning(conversation string) bool {
	dir, prefix := l.waitLocks(conversation)
	entries, _ := os.ReadDir(dir)
	running := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".lock") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
			running = true
		} else {
			syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			os.Remove(p)
		}
		f.Close()
	}
	return running
}

// LockRedeems serializes the harness's redeems on this machine until the
// returned func: each reads the link the conversation holds for the app,
// redeems with it (held) and saves the new one. Two results of one turn
// (tool calls in parallel, whose hooks run at once) each carry a beacon,
// for two sessions when the calls came before either result gave the
// conversation a session_key: one after the other, the second redeem hands
// back the first's link, and Wakeshift keeps the conversation one session
// (appapi's mergeInto and foldInto). Without the harness's directory
// (uninstalled) it does nothing.
func (l Links) LockRedeems() func() {
	dir := filepath.Dir(l.Dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return func() {}
	}
	unlock, err := lockFile(filepath.Join(dir, "redeem.lock"))
	if err != nil {
		return func() {}
	}
	return unlock
}

// Forget removes all of a conversation's links.
func (l Links) Forget(conversation string) {
	os.Remove(l.path(conversation))
	os.RemoveAll(l.appDir(conversation))
}

// ForgetLink removes the conversation's link link (revoked or expired),
// leaving its other apps' links.
func (l Links) ForgetLink(conversation, link string) {
	if s, ok := l.legacy(conversation); ok && s.Link == link {
		os.Remove(l.path(conversation))
	}
	for _, k := range l.All(conversation) {
		if k.Link == link && k.App != "" {
			os.Remove(l.appPath(conversation, k.App))
		}
	}
	os.Remove(l.appDir(conversation)) // only once it's empty
}

// Prompt frames deliveries for the model as outside data (internal/framing):
// what a hook hands the harness (as context after a tool call, or as the
// input a stop hook continues the session with).
func Prompt(ds []Delivery) string {
	ms := make([]framing.Message, len(ds))
	for i, d := range ds {
		ms[i] = framing.FromWire(d.Event, d.App).Message()
	}
	return framing.Frame("", ms...)
}

// prompt is Prompt, after framing.Headline for a harness whose hook's
// first line is what a wake opens with (Hook.Headline).
func (h *Hook) prompt(ds []Delivery) string {
	if !h.Headline || len(ds) == 0 {
		return Prompt(ds)
	}
	es := make([]framing.Event, len(ds))
	for i, d := range ds {
		es[i] = framing.FromWire(d.Event, d.App)
	}
	return framing.Headline(es...) + "\n" + Prompt(ds)
}
