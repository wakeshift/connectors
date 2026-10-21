package codexconnector

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/clawvisor/wakeshift-plugins/internal/connectorversion"
	"github.com/clawvisor/wakeshift-plugins/internal/version"
)

// The connector's stream names it (X-Wakeshift-Connector), and a 426
// update_required ends it (ErrOutdated) rather than reconnecting: the
// thread's next tool result asks for the update.
func TestStreamStopsWhenOutdated(t *testing.T) {
	was := version.Version
	version.Version = "2026.10.20.1435+abc1234"
	defer func() { version.Version = was }()
	got := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(connectorversion.Header)
		w.WriteHeader(http.StatusUpgradeRequired)
		w.Write([]byte(`{"error":{"code":"update_required","message":"out of date"}}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := &conn{cfg: Config{Stream: "ws" + strings.TrimPrefix(srv.URL, "http"), Log: slog.New(slog.DiscardHandler)}, cancel: cancel,
		seen: map[string]bool{}, unconfirmed: map[string]bool{}}
	done := make(chan struct{})
	go func() { c.stream(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream went on")
	}
	if !errors.Is(context.Cause(ctx), ErrOutdated) {
		t.Fatalf("stopped with %v", context.Cause(ctx))
	}
	if h := <-got; h != "wakeshift/2026.10.20.1435+abc1234 codex" || len(got) != 0 {
		t.Fatalf("header %q, %d more tries", h, len(got))
	}
}

// A live stream closed with CodeUpdateRequired (the minimum was raised)
// ends the connector too.
func TestStreamClosedAsOutdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.Close(4002, "update_required")
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c := &conn{cfg: Config{Stream: "ws" + strings.TrimPrefix(srv.URL, "http"), Log: slog.New(slog.DiscardHandler)}, cancel: cancel,
		seen: map[string]bool{}, unconfirmed: map[string]bool{}}
	done := make(chan struct{})
	go func() { c.stream(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream went on")
	}
	if !errors.Is(context.Cause(ctx), ErrOutdated) {
		t.Fatalf("stopped with %v", context.Cause(ctx))
	}
}
