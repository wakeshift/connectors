package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/beacon"
	"github.com/wakeshift/connectors/internal/connectorversion"
	"github.com/wakeshift/connectors/internal/version"
)

// Every request the hooks make names the binary, its version and the
// harness (X-Wakeshift-Connector), and a 426 update_required is an
// *UpdateRequired with Wakeshift's message: the poll keeps the link, the
// hook's note says what to tell the user.
func TestRequestsNameTheConnector(t *testing.T) {
	was := version.Version
	version.Version = "2026.10.20.1435+abc1234"
	defer func() { version.Version = was }()
	var mu sync.Mutex
	got := map[string]string{}
	outdated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = r.Header.Get(connectorversion.Header)
		refuse := outdated
		mu.Unlock()
		if refuse {
			w.WriteHeader(http.StatusUpgradeRequired)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "update_required", "message": "Wakeshift's hooks for Cursor are out of date. Ask the user."}})
			return
		}
		switch r.URL.Path {
		case "/v1/connector/redeem":
			json.NewEncoder(w).Encode(map[string]string{"url": "ws://" + r.Host + "/v1/stream?link=tok"})
		case "/v1/stream/poll":
			json.NewEncoder(w).Encode(map[string]any{"deliveries": []any{}})
		case "/v1/stream/ack":
			json.NewEncoder(w).Encode(map[string]any{"acked": []string{"d1"}})
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	claims := beacon.Claims{Ticket: "t", Redeem: srv.URL + "/v1/connector/redeem"}
	link, err := RedeemCall(ctx, nil, "cursor", claims, "conv", "call")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := Lookup("cursor")
	p := spec.NewHook(nil, nil).Poller
	if _, err := p.Poll(ctx, link, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Ack(ctx, link, []string{"d1"}); err != nil {
		t.Fatal(err)
	}
	const want = "wakeshift/2026.10.20.1435+abc1234 cursor"
	for _, k := range []string{"POST /v1/connector/redeem", "GET /v1/stream/poll", "POST /v1/stream/ack"} {
		if got[k] != want {
			t.Errorf("%s sent %q, want %q", k, got[k], want)
		}
	}

	mu.Lock()
	outdated = true
	mu.Unlock()
	_, err = RedeemCall(ctx, nil, "cursor", claims, "conv", "call")
	var u *UpdateRequired
	if !errors.As(err, &u) || !strings.HasPrefix(u.Message, "Wakeshift's hooks for Cursor are out of date.") {
		t.Fatalf("a refused redeem: %v", err)
	}
	if _, err := p.Poll(ctx, link, 0, false); !errors.As(err, &u) {
		t.Fatalf("a refused poll: %v", err)
	}
}

// A hook refused as out of date says so in its note, with Wakeshift's
// words, and a listener stops without forgetting the link.
func TestOutdatedHookNotes(t *testing.T) {
	if n := NoteOutdated; !strings.HasPrefix(n, "Wakeshift: couldn't connect this conversation. ") {
		t.Fatalf("note %q", n)
	}
	links := LinksFor(Settings{Dir: t.TempDir()})
	links.Save("conv", "ws://x/v1/stream?link=tok")
	h := &Hook{Poller: &outdatedPoller{}, Links: links}
	if out, _ := h.take(context.Background(), "conv", 0, true); out != "" {
		t.Fatalf("handed over %q", out)
	}
	if links.Load("conv") == "" || h.why != "Wakeshift's hooks are out of date" {
		t.Fatalf("link %q, why %q", links.Load("conv"), h.why)
	}
}

// An update (the install run again over an install) keeps what the user
// has: the hooks file byte for byte (so Codex, which trusts the hook's
// command, needn't trust it again), the Wakeshifts trusted, and the
// conversations' links.
func TestUpdateKeepsSettingsAndLinks(t *testing.T) {
	for _, name := range []string{"codex", "claude-code", "cursor"} {
		home := setHome(t)
		t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
		spec, _ := Lookup(name)
		if err := spec.Install("/home/u/.wakeshift/bin/wakeshift", "https://app.wakeshift.ai", &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		f, ok := spec.Installer.(hookFiler)
		if !ok {
			t.Fatalf("%s: no hooks file", name)
		}
		before, _ := os.ReadFile(f.path())
		set := LoadSettings(name)
		set.Trust("https://other.wakeshift.example")
		LinksFor(set).Save("conv-1", "wss://app.wakeshift.ai/v1/stream?link=tok")
		var out bytes.Buffer
		if err := spec.Install("/home/u/.wakeshift/bin/wakeshift", "https://app.wakeshift.ai", &out); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(f.path())
		if !bytes.Equal(before, after) {
			t.Errorf("%s: the update changed the hooks file:\n%s\n%s", name, before, after)
		}
		if s := LoadSettings(name); len(s.Issuers) != 2 {
			t.Errorf("%s: trusted %v after the update", name, s.Issuers)
		}
		if LinksFor(LoadSettings(name)).Load("conv-1") == "" {
			t.Errorf("%s: the update lost a conversation's link", name)
		}
	}
}

type outdatedPoller struct{}

func (outdatedPoller) Poll(context.Context, string, time.Duration, bool) (Batch, error) {
	return Batch{}, &UpdateRequired{Message: "out of date"}
}
func (outdatedPoller) Ack(context.Context, string, []string) error { return nil }
