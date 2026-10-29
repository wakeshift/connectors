package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/connectors/internal/framing"
)

// Links keeps each conversation's stream link (a secret: 0600 files in a
// 0700 directory), named by a hash of the conversation ID so odd IDs can't
// escape the directory.
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

func (l Links) path(conversation string) string {
	sum := sha256.Sum256([]byte(conversation))
	return filepath.Join(l.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Save keeps a conversation's link (a rotated one: the app stays as it was).
func (l Links) Save(conversation, link string) error {
	return l.SaveFor(conversation, link, l.load(conversation).App)
}

// SaveFor keeps a conversation's link, from app's beacon.
func (l Links) SaveFor(conversation, link, app string) error {
	if conversation == "" || link == "" {
		return errors.New("connector: saving an empty link")
	}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(savedLink{Conversation: conversation, Link: link, App: app, SavedAt: time.Now().UTC()})
	tmp := l.path(conversation) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path(conversation))
}

// Load returns a conversation's link ("" without one).
func (l Links) Load(conversation string) string { return l.load(conversation).Link }

// AppOf is the app whose beacon connected the conversation ("" if none, or
// a link saved before links named their app).
func (l Links) AppOf(conversation string) string { return l.load(conversation).App }

func (l Links) load(conversation string) savedLink {
	if conversation == "" {
		return savedLink{}
	}
	b, err := os.ReadFile(l.path(conversation))
	if err != nil {
		return savedLink{}
	}
	var s savedLink
	if json.Unmarshal(b, &s) != nil || s.Conversation != conversation {
		return savedLink{}
	}
	return s
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

// Forget removes a conversation's link.
func (l Links) Forget(conversation string) {
	os.Remove(l.path(conversation))
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
