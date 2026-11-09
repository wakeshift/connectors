package codexconnector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wakeshift/connectors/internal/connector"
)

// Codex's hook keeps and starts only a link on the beacon's issuer's
// origin; the connector dials only a link on a trusted issuer's origin, and
// never follows a redirect off it.
func TestCodexLinksStayOnTheIssuersOrigin(t *testing.T) {
	var discard bytes.Buffer
	t.Cleanup(connector.RefusalOutput(&discard))
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("WAKESHIFT_HOME", t.TempDir())
	var hits atomic.Int32
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer evil.Close()
	evilWS := "ws" + strings.TrimPrefix(evil.URL, "http") + "/v1/stream?link=SECRET"

	// The hook: a redeem answering another origin starts nothing.
	ws := newFakeWakeshift(t)
	ws.answer = evilWS
	var started []string
	h := &Hook{Settings: Settings{Dir: t.TempDir(), Issuers: []string{ws.URL}}, Socket: fakeAppServer(t, thread),
		Start: func(th, link, _, _ string, _ bool) error { started = append(started, th+" "+link); return nil }}
	var out bytes.Buffer
	h.Run(context.Background(), hookInput(thread, ws.beacon(t, thread, nil)), &out)
	if got := noteOf(t, out.String()); !strings.Contains(got, "refused a stream link") || strings.Contains(got, "SECRET") || len(started) != 0 {
		t.Fatalf("note %q, started %v", got, started)
	}
	if l := h.links().Load(thread); l != "" {
		t.Fatalf("kept %q", l)
	}

	// The connector: an off-origin link (a kept file someone changed) is
	// never dialed.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, Config{Stream: evilWS, Issuers: []string{"https://app.wakeshift.ai"}, Thread: thread, Socket: fakeAppServer(t, thread),
		StartGrace: time.Second, Log: slog.New(slog.DiscardHandler)})
	var off *connector.ErrOffOrigin
	if !errors.As(err, &off) {
		t.Fatalf("Run: %v", err)
	}
	if err := Run(ctx, Config{Stream: evilWS, Thread: thread, Socket: fakeAppServer(t, thread),
		StartGrace: time.Second, Log: slog.New(slog.DiscardHandler)}); !errors.As(err, &off) {
		t.Fatalf("Run trusting nothing: %v", err)
	}

	// An on-origin link whose server redirects elsewhere: not followed.
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	c := &conn{cfg: Config{Stream: "ws" + strings.TrimPrefix(redirecting.URL, "http") + "/v1/stream?link=SECRET", Issuers: []string{redirecting.URL},
		Log: slog.New(slog.DiscardHandler)}, cancel: func(error) {}}
	if _, err := c.streamOnce(ctx, c.cfg.Stream); err == nil {
		t.Fatal("a redirected dial connected")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("reached the other server %d times", n)
	}
}
