package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// What the plugin sends the bridge for a Wakeshift tool result: opencode's
// real tool.execute.after output (1.18.7), the CallToolResult with _meta.
func opencodeLine(session, token string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "session_id": session, "call_id": "call_87f5",
		"tool_name": "chat_watch", "tool_response": map[string]any{"_meta": map[string]any{"wakeshift/connector": token},
			"content": []any{map[string]string{"type": "text", "text": "ok"}}, "structuredContent": map[string]any{"ok": true}}})
	return string(b) + "\n"
}

func TestOpencodeBridge(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := newFakePoller()
	set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
	spec, _ := Lookup("opencode")
	h := &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When,
		Unbound: spec.Unbound, Repoll: 50 * time.Millisecond}

	// Not ours: the bridge answers and exits.
	var out bytes.Buffer
	(&Bridge{Hook: h}).Run(context.Background(), strings.NewReader(opencodeLine("ses_1", "a.b.c")), &out)
	if strings.TrimSpace(out.String()) != `{"additionalContext":"","keep_alive":false}` {
		t.Fatalf("not ours: %s", out.String())
	}

	// An unbound beacon (opencode tells servers nothing) connects the session
	// the plugin names; the bridge stays, hands over a message, and acks it
	// only on the plugin's ack.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&Bridge{Hook: h}).Run(context.Background(), inR, outW); outW.Close() }()
	go io.WriteString(inW, opencodeLine("ses_1", unbound(t, ws)))
	lines := bufio.NewScanner(outR)
	lines.Scan()
	var first struct {
		AdditionalContext string `json:"additionalContext"`
		KeepAlive         bool   `json:"keep_alive"`
	}
	json.Unmarshal(lines.Bytes(), &first)
	if !first.KeepAlive || !strings.Contains(first.AdditionalContext, "Wakeshift: connected") {
		t.Fatalf("first line %s", lines.Bytes())
	}
	if r := ws.redemptions(); len(r) != 1 || r[0] != "wst_t@ses_1" {
		t.Fatalf("redeemed %v", r)
	}
	link := h.Links.Load("ses_1")
	p.add(link, msg("d1", "hello opencode"))
	lines.Scan()
	var wake struct {
		Wake struct{ ID, Text string }
	}
	json.Unmarshal(lines.Bytes(), &wake)
	if wake.Wake.ID == "" || !strings.Contains(wake.Wake.Text, "hello opencode") {
		t.Fatalf("wake %s", lines.Bytes())
	}
	time.Sleep(50 * time.Millisecond)
	if a := p.ackedNow(); len(a) != 0 {
		t.Fatalf("acked %v before the plugin did", a)
	}
	io.WriteString(inW, `{"ack":"not-this-one"}`+"\n")
	io.WriteString(inW, `{"ack":"`+wake.Wake.ID+`"}`+"\n")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if a := p.ackedNow(); len(a) > 0 {
			if !slices.Equal(a, []string{"d1"}) {
				t.Fatalf("acked %v, want d1", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not acked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	inW.Close() // opencode quit
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the bridge outlived opencode")
	}

	// After a restart, an idle session with a link listens again.
	out.Reset()
	in := `{"hook_event_name":"SessionIdle","session_id":"ses_1"}` + "\n"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	(&Bridge{Hook: h}).Run(ctx, strings.NewReader(in), &out)
	if !strings.Contains(out.String(), `"keep_alive":true`) {
		t.Fatalf("idle: %s", out.String())
	}
}

// A message waiting when the bridge connects goes in the tool result's note
// and is acknowledged once the plugin says it added the note: it doesn't
// come again as a wake (wakebench S8, October 26: after a restart the held
// message came in the watch result and as a wake, "GOT … (duplicate)").
// Without the plugin's ack (a plugin from before note_id), it comes again
// as a wake: at least once.
func TestOpencodeBridgeNoteDeliversOnce(t *testing.T) {
	for _, pluginAcks := range []bool{true, false} {
		t.Run(map[bool]string{true: "plugin acks the note", false: "no ack"}[pluginAcks], func(t *testing.T) {
			ws := newFakeWakeshift(t)
			p := newFakePoller()
			set := Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}
			spec, _ := Lookup("opencode")
			h := &Hook{Format: spec.Format, Verifier: &Verifier{Settings: set}, Poller: p, Links: LinksFor(set), When: spec.When,
				Unbound: spec.Unbound, Repoll: 50 * time.Millisecond}
			// The link the fake redeems for ses_2, with a message already waiting on it.
			link := "ws://" + strings.TrimPrefix(ws.URL, "http://") + "/v1/stream?link=tok-ses_2"
			p.add(link, msg("d1", "held while opencode was closed"))
			acked := func() []string {
				p.mu.Lock()
				defer p.mu.Unlock()
				return append([]string(nil), p.acked...)
			}

			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- (&Bridge{Hook: h, NoteTimeout: 300 * time.Millisecond}).Run(ctx, inR, outW)
				outW.Close()
			}()
			go io.WriteString(inW, opencodeLine("ses_2", unbound(t, ws)))
			lines := bufio.NewScanner(outR)
			if !lines.Scan() {
				t.Fatal("no first line")
			}
			var first struct {
				AdditionalContext string `json:"additionalContext"`
				KeepAlive         bool   `json:"keep_alive"`
				NoteID            string `json:"note_id"`
			}
			json.Unmarshal(lines.Bytes(), &first)
			if got := h.Links.Load("ses_2"); got != link {
				t.Fatalf("link %q, want %q", got, link)
			}
			if !first.KeepAlive || first.NoteID == "" || !strings.Contains(first.AdditionalContext, "held while opencode was closed") {
				t.Fatalf("first line %s", lines.Bytes())
			}
			if a := acked(); len(a) != 0 {
				t.Fatalf("acked %v before the plugin added the note", a)
			}
			wakes := make(chan string, 4)
			go func() {
				for lines.Scan() {
					wakes <- lines.Text()
				}
			}()
			if pluginAcks {
				io.WriteString(inW, `{"ack":"`+first.NoteID+`"}`+"\n")
				deadline := time.Now().Add(2 * time.Second)
				for len(acked()) == 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond)
				}
				if a := acked(); len(a) != 1 || a[0] != "d1" {
					t.Fatalf("acked %v after the plugin's ack, want [d1]", a)
				}
				select {
				case w := <-wakes:
					t.Fatalf("delivered again as a wake: %s", w)
				case <-time.After(600 * time.Millisecond):
				}
			} else {
				select {
				case w := <-wakes:
					if !strings.Contains(w, "held while opencode was closed") {
						t.Fatalf("wake %s", w)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("an unacknowledged note's message never came again")
				}
			}
			inW.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the bridge outlived opencode")
			}
		})
	}
}

func TestOpencodeInstall(t *testing.T) {
	home := setHome(t)
	spec, _ := Lookup("opencode")
	const bin = "/Users/ada/.wakeshift/bin/wakeshift"
	var out bytes.Buffer
	if err := spec.Install(bin, "https://app.wakeshift.ai", &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "opencode", "plugins", "wakeshift.ts")
	b, err := os.ReadFile(path)
	if err != nil || !has(string(b), `const BIN = "/Users/ada/.wakeshift/bin/wakeshift"`, "export default {", `"bridge", "--harness", "opencode"`) {
		t.Fatalf("%v %s", err, b)
	}
	// Only a default export: opencode refuses other named exports.
	if strings.Contains(string(b), "export const") || strings.Contains(string(b), "export function") {
		t.Fatal("a named export")
	}
	out.Reset()
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if !has(out.String(), "already") {
		t.Fatalf("twice: %s", out.String())
	}
	// A new binary path rewrites it, keeping the old one.
	out.Reset()
	spec.Install("/opt/wakeshift", "https://app.wakeshift.ai", &out)
	if !has(out.String(), ".before-wakeshift-") {
		t.Fatalf("rewrite: %s", out.String())
	}
	if err := spec.Uninstall(bin, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("still there")
	}
	// Someone else's file at that path isn't removed.
	os.WriteFile(path, []byte("export default {}"), 0o600)
	if err := spec.Uninstall(bin, &out); err == nil {
		t.Fatal("removed a file that isn't ours")
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	spec.Install(bin, "https://app.wakeshift.ai", &out)
	if _, err := os.Stat(filepath.Join(home, "xdg", "opencode", "plugins", "wakeshift.ts")); err != nil {
		t.Fatal("XDG_CONFIG_HOME ignored")
	}
}
