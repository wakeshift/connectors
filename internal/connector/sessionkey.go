package connector

// The per-session keys of redeem by call (callredeem.go, claimd.go): made,
// read, and swept once their session is over. A key file is the session's
// credential for its calls' claims and redeems, so it lives 0600 in a 0700
// directory, and goes when no session will use it again.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func short(prefix, s string) string {
	h := sha256.Sum256([]byte(prefix + "\x00" + s))
	return hex.EncodeToString(h[:16])
}

func sessionDir(dir, session string) string {
	return filepath.Join(dir, "sessions", short("wakeshift-session", session))
}

// sessionKey is the Claude Code session's key, made if create and there's
// none: its seed, base64url, in <dir>/sessions/<hash>/key (0600, the
// directory 0700), written to a temporary file and linked into place, so
// two hooks making it at once both read the one that won and a reader
// never sees half a file. transcript, when it's made: the session's
// transcript, beside it, for the sweep.
func sessionKey(dir, session string, create bool, transcript string) (ed25519.PrivateKey, error) {
	sd := sessionDir(dir, session)
	path := filepath.Join(sd, "key")
	if k, err := readSessionKey(path); err == nil || !create {
		if err == nil && create {
			// Used: the sweep keeps a key a live session uses, whatever
			// its transcript's time says.
			now := time.Now()
			os.Chtimes(path, now, now)
		}
		return k, err
	}
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(sd, ".key-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.WriteString(base64.RawURLEncoding.EncodeToString(seed) + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil {
		return nil, errors.Join(werr, cerr)
	}
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	if transcript != "" && filepath.IsAbs(transcript) {
		// The first record stays: two hooks making the key at once don't
		// overwrite each other's (the sweep reads it).
		if f, err := os.OpenFile(filepath.Join(sd, "transcript"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			f.WriteString(transcript + "\n")
			f.Close()
		}
	}
	return readSessionKey(path)
}

func readSessionKey(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seed, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("connector: the session key is unreadable")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// keyIdle: a session key whose transcript hasn't changed for this long is
// swept (a resume after that makes a new key, which the core binds nothing
// to for a conversation it bound to the old one: the beacon connects it).
const keyIdle = 30 * 24 * time.Hour

// SweepSessionKeys deletes the session keys (and claim records) under dir
// that no session will use again: a key whose transcript is gone, or
// unchanged for keyIdle (a key without a transcript record, by its own
// age), and claim records over an hour old. At most once a day per
// directory (a marker file); a waiter runs it, never a synchronous hook.
// It returns how many keys it deleted.
func SweepSessionKeys(dir string, now time.Time) int {
	marker := filepath.Join(dir, "sessions", ".swept")
	if st, err := os.Stat(marker); err == nil && now.Sub(st.ModTime()) < 24*time.Hour {
		return 0
	}
	n := 0
	entries, _ := os.ReadDir(filepath.Join(dir, "sessions"))
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 32 {
			continue
		}
		sd := filepath.Join(dir, "sessions", e.Name())
		key, err := os.Stat(filepath.Join(sd, "key"))
		if err != nil {
			continue
		}
		last := key.ModTime()
		gone := false
		if b, err := os.ReadFile(filepath.Join(sd, "transcript")); err == nil {
			st, err := os.Stat(strings.TrimSpace(string(b)))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				gone = now.Sub(key.ModTime()) > time.Hour // not yet written, for a brand-new session
			case err == nil && st.ModTime().After(last):
				last = st.ModTime() // the key's own time is its last use
			}
		}
		if gone || now.Sub(last) > keyIdle {
			if os.RemoveAll(sd) == nil {
				n++
			}
		}
	}
	claims, _ := os.ReadDir(filepath.Join(dir, "claims"))
	for _, e := range claims {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(dir, "claims", e.Name()))
		}
	}
	// The relays' sockets (and their locks) stay when a waiter stops, one
	// per Claude Code process: those untouched for keyIdle go. A waiter
	// makes its socket again each time it starts.
	relays, _ := os.ReadDir(filepath.Join(dir, "claimd"))
	for _, e := range relays {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > keyIdle {
			os.Remove(filepath.Join(dir, "claimd", e.Name()))
		}
	}
	if os.MkdirAll(filepath.Dir(marker), 0o700) == nil {
		os.WriteFile(marker, nil, 0o600)
		os.Chtimes(marker, now, now)
	}
	return n
}
