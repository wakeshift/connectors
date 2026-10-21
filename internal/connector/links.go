package connector

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
