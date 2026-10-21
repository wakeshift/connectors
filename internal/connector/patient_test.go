package connector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// bgPoller answers Background (the agent's wait command holds the stream)
// to its first polls, then a delivery or an error, and records whether
// each poll was patient.
type bgPoller struct {
	mu         sync.Mutex
	background int
	then       error
	patient    []bool
	acked      []string
}

func (p *bgPoller) Poll(ctx context.Context, link string, wait time.Duration, patient bool) (Batch, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.patient = append(p.patient, patient)
	if p.background > 0 {
		p.background--
		return Batch{Background: true}, nil
	}
	if p.then != nil {
		return Batch{}, p.then
	}
	return Batch{Deliveries: []Delivery{msg("d1", "posted while the command was dead")}}, nil
}

func (p *bgPoller) Ack(ctx context.Context, link string, ids []string) error {
	p.mu.Lock()
	p.acked = append(p.acked, ids...)
	p.mu.Unlock()
	return nil
}

// A waiter (Claude Code's rewake hook) doesn't stand down for the agent's
// wait command: it polls patiently, so the server takes the stream for it
// once that command stops counting as live, and it hands over what comes.
// (Right after the hooks were installed, waiters exited within seconds for
// minutes because a stopped Monitor still looked live, and a post was
// lost.) A stop hook still stands down at once, so the turn ends. Each
// waiter logs why it stopped, with no link in the log.
func TestWaiterOutlastsTheAgentsWaitCommand(t *testing.T) {
	ws := newFakeWakeshift(t)
	p := &bgPoller{background: 3}
	h := testHook(t, ws, newFakePoller())
	h.Poller, h.Repoll = p, 20*time.Millisecond
	link := "wss://wakeshift.test/v1/stream?link=SECRETTOKEN"
	h.Links.Save("c", link)
	if out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"}); out != "" || len(p.patient) != 1 || p.patient[0] {
		t.Fatalf("a stop hook: %q, patient %v", out, p.patient)
	}
	p.patient = nil
	h.Rewake = true
	out, _ := call(h, map[string]any{"event": "stop", "conversation": "c"})
	if !strings.Contains(out, "posted while the command was dead") || len(p.acked) != 1 {
		t.Fatalf("the waiter: %q, acked %v", out, p.acked)
	}
	for _, patient := range p.patient {
		if !patient {
			t.Fatalf("a waiter's poll wasn't patient: %v", p.patient)
		}
	}

	// It logs why, and nothing secret.
	p.then = ErrReplaced
	h2 := testHook(t, ws, newFakePoller())
	h2.Verifier, h2.Links, h2.Poller, h2.Rewake = h.Verifier, h.Links, p, true
	call(h2, map[string]any{"event": "stop", "conversation": "c"})
	path := WaiterLog(h.Verifier.Settings.Dir)
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the log: %v %v", st, err)
	}
	if d, _ := os.Stat(filepath.Dir(path)); d.Mode().Perm() != 0o700 {
		t.Fatalf("the log's directory: %v", d.Mode())
	}
	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `exit="handed messages over"`) || !strings.Contains(lines[1], `exit="another poll took the stream (a newer hook)"`) ||
		strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "wss://") || strings.Contains(string(b), "conversation=c ") {
		t.Fatalf("the log:\n%s", b)
	}
}
