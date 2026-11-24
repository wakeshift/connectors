package codexconnector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Background turns: a desktop thread that no window of the app has loaded
// is resumed in Codex's documented app-server (`codex app-server --listen
// stdio://`, newline-delimited JSON-RPC), run for one turn whose input is a
// tool output (outside data, never the user's words), and closed.

// Permissions for a background turn (the user's choice: SetBackground).
const (
	// PermissionsReadOnly (the default): read-only sandbox. Whatever the
	// permissions, a background turn runs with backgroundApproval.
	PermissionsReadOnly = "read-only"
	// PermissionsWorkspaceWrite: the thread's workspace is writable.
	PermissionsWorkspaceWrite = "workspace-write"
	// PermissionsThread: the sandbox the thread's last turn ran with.
	PermissionsThread = "thread"
)

// backgroundApproval is the approval policy of every background turn. Codex
// asks for approval before running any command but its known-safe
// read-only ones, and the connector refuses every request (handle): nobody
// is there to approve, and the turn's input is outside data that may carry
// a prompt injection. So in a background turn, a command that needs
// approval fails; the model sees that it failed and can still reply, and
// the turn still completes, which acknowledges its events. It's set on
// thread/resume and turn/start, whatever the user's config.toml says.
//
// Codex 0.171 refuses "untrusted" as a config value (approval_policy = ...
// "is no longer supported") and exits, though its app-server protocol still
// takes it, so the process gets processApproval (never "never"). A Codex
// that refuses it in the requests too fails the turn (errApprovalRefused):
// the events are kept and tried again, with a warning in the log.
const backgroundApproval = "untrusted"

// processApproval is the approval policy set for the app-server process.
const processApproval = "on-request"

// background is the user's choice for background turns, in the
// connector's settings directory (background.json).
type background struct {
	Permissions string `json:"permissions"`
}

func backgroundPath(dir string) string { return filepath.Join(dir, "background.json") }

// BackgroundPermissions is the user's choice for background turns
// (PermissionsReadOnly unless they chose otherwise).
func BackgroundPermissions(dir string) string {
	var b background
	if raw, err := os.ReadFile(backgroundPath(dir)); err == nil && json.Unmarshal(raw, &b) == nil {
		switch b.Permissions {
		case PermissionsWorkspaceWrite, PermissionsThread:
			return b.Permissions
		}
	}
	return PermissionsReadOnly
}

// SetBackgroundPermissions records the user's choice.
func SetBackgroundPermissions(dir, p string) error {
	switch p {
	case PermissionsReadOnly, PermissionsWorkspaceWrite, PermissionsThread:
	default:
		return fmt.Errorf("permissions must be %s, %s or %s", PermissionsReadOnly, PermissionsWorkspaceWrite, PermissionsThread)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(background{Permissions: p})
	return os.WriteFile(backgroundPath(dir), b, 0o600)
}

// sandboxFor is the sandbox mode a background turn runs with: perms, or
// for PermissionsThread the thread's own (its last turn's), never more than
// workspace-write unless the thread itself ran with full access.
func sandboxFor(perms, threadSandbox string) string {
	switch perms {
	case PermissionsWorkspaceWrite:
		return "workspace-write"
	case PermissionsThread:
		switch threadSandbox {
		case "read-only", "workspace-write", "danger-full-access":
			return threadSandbox
		}
	}
	return "read-only"
}

// sandboxPolicy is turn/start's form of a sandbox mode.
func sandboxPolicy(mode string) map[string]any {
	switch mode {
	case "workspace-write":
		return map[string]any{"type": "workspaceWrite"}
	case "danger-full-access":
		return map[string]any{"type": "dangerFullAccess"}
	}
	return map[string]any{"type": "readOnly"}
}

// errThreadInUse: another Codex process has the thread loaded (Codex lets
// one process write a thread: CODEX_HOME/thread-writer-locks). The desktop
// app's own Codex holds it while the thread is loaded there, even when no
// window owns it. The connector never touches the lock: it waits.
var errThreadInUse = errors.New("another Codex process has the thread loaded")

// errTurnFailed: the turn ran and failed (the model, say): its events are
// kept for the next try.
var errTurnFailed = errors.New("the turn failed")

// errApprovalRefused: Codex refused backgroundApproval in a request (a
// version that dropped it). The turn fails (errTurnFailed) rather than run
// with a looser policy, so its events are kept and tried again.
var errApprovalRefused = errors.New("Codex refused the background turn's approval policy (" + backgroundApproval + ")")

// refusedApproval: err is Codex refusing a request's approvalPolicy.
func refusedApproval(err error) bool {
	var re *rpcError
	if !errors.As(err, &re) {
		return false
	}
	m := strings.ToLower(re.Message)
	return strings.Contains(m, backgroundApproval) || strings.Contains(m, "approvalpolicy") || strings.Contains(m, "approval_policy")
}

// backgroundTurn is one background turn's request.
type backgroundTurn struct {
	Thread  string
	Cwd     string // the thread's (its rollout's session_meta)
	Sandbox string // read-only, workspace-write or danger-full-access
	Output  string // the tool output: the framed events
}

// AppServerTurn runs one turn with bin's app-server: errThreadInUse if
// another process has the thread, errTurnFailed if the turn failed.
func AppServerTurn(bin string) func(ctx context.Context, t backgroundTurn) error {
	return func(ctx context.Context, t backgroundTurn) error {
		if !uuidRE.MatchString(t.Thread) {
			return errors.New("not a Codex thread ID")
		}
		// The sandbox and approval policy are set for the process too, so
		// the user's config.toml can't change them: not widen the sandbox
		// (danger-full-access, say), nor turn approvals off.
		cmd := exec.Command(bin, "-c", fmt.Sprintf("sandbox_mode=%q", t.Sandbox), "-c", fmt.Sprintf("approval_policy=%q", processApproval),
			"app-server", "--listen", "stdio://")
		if t.Cwd != "" {
			if st, err := os.Stat(t.Cwd); err == nil && st.IsDir() {
				cmd.Dir = t.Cwd
			}
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.Stderr = io.Discard // the conversation, at times: never kept
		cmd.WaitDelay = 5 * time.Second
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("codex app-server: %w", err)
		}
		defer func() {
			stdin.Close()
			done := make(chan struct{})
			go func() { cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}()
		s := newStdioServer(stdin, stdout)
		go s.read()
		return s.turn(ctx, t)
	}
}

// stdioServer is a JSON-RPC client for an app-server on stdio.
type stdioServer struct {
	w      io.Writer
	r      *bufio.Reader
	wmu    sync.Mutex
	next   atomic.Int64
	mu     sync.Mutex
	wait   map[int64]chan rpcReply
	notes  chan note
	done   chan struct{}
	closed atomic.Bool
}

func newStdioServer(w io.Writer, r io.Reader) *stdioServer {
	return &stdioServer{w: w, r: bufio.NewReaderSize(r, 1<<20), wait: map[int64]chan rpcReply{}, notes: make(chan note, 1024), done: make(chan struct{})}
}

func (s *stdioServer) send(m map[string]any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.w.Write(append(b, '\n'))
	return err
}

func (s *stdioServer) read() {
	defer close(s.done)
	for {
		line, err := s.r.ReadBytes('\n')
		if len(line) > 0 {
			s.handle(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *stdioServer) handle(line []byte) {
	var m struct {
		ID     *json.RawMessage `json:"id"`
		Method string           `json:"method"`
		Params json.RawMessage  `json:"params"`
		Result json.RawMessage  `json:"result"`
		Error  *rpcError        `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	switch {
	case m.ID != nil && m.Method != "":
		// An approval or question: nobody is here to answer, so it's
		// refused and what asked fails closed (backgroundApproval).
		s.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "the Wakeshift connector can't answer interactive requests"}})
	case m.ID != nil:
		var id int64
		json.Unmarshal(*m.ID, &id)
		s.mu.Lock()
		ch := s.wait[id]
		delete(s.wait, id)
		s.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{Result: m.Result, Err: m.Error}
		}
	case m.Method != "":
		select {
		case s.notes <- note{m.Method, m.Params}:
		default:
		}
	}
}

func (s *stdioServer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.next.Add(1)
	ch := make(chan rpcReply, 1)
	s.mu.Lock()
	s.wait[id] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.wait, id); s.mu.Unlock() }()
	if err := s.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("codex app-server: %w", err)
	}
	select {
	case r := <-ch:
		if r.Err != nil {
			return nil, r.Err
		}
		return r.Result, nil
	case <-s.done:
		return nil, errors.New("codex app-server exited")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// turn resumes the thread, starts the turn and waits for it to end. On
// ctx's end it interrupts the turn rather than leave it running unseen.
func (s *stdioServer) turn(ctx context.Context, t backgroundTurn) error {
	if _, err := s.call(ctx, "initialize", map[string]any{"clientInfo": map[string]any{
		"name": "wakeshift", "title": "Wakeshift connector", "version": "0.0.1"}}); err != nil {
		return fmt.Errorf("codex app-server: initialize: %w", err)
	}
	if err := s.send(map[string]any{"method": "initialized"}); err != nil {
		return err
	}
	resume := map[string]any{"threadId": t.Thread, "sandbox": t.Sandbox, "approvalPolicy": backgroundApproval, "excludeTurns": true}
	if _, err := s.call(ctx, "thread/resume", resume); err != nil {
		if strings.Contains(err.Error(), "active writer") {
			return errThreadInUse
		}
		if refusedApproval(err) {
			return fmt.Errorf("%w: %w: thread/resume: %v", errTurnFailed, errApprovalRefused, err)
		}
		return fmt.Errorf("codex app-server: thread/resume: %w", err)
	}
	r, err := s.call(ctx, "turn/start", map[string]any{"threadId": t.Thread, "input": []any{},
		"approvalPolicy": backgroundApproval, "sandboxPolicy": sandboxPolicy(t.Sandbox),
		"toolOutput": map[string]any{"name": "wakeshift_event", "output": t.Output}})
	if err != nil {
		if refusedApproval(err) {
			return fmt.Errorf("%w: %w: turn/start: %v", errTurnFailed, errApprovalRefused, err)
		}
		return fmt.Errorf("%w: turn/start: %v", errTurnFailed, err)
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	json.Unmarshal(r, &started)
	for {
		select {
		case n := <-s.notes:
			if n.Method != "turn/completed" {
				continue
			}
			var p struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(n.Params, &p) != nil || p.ThreadID != t.Thread || started.Turn.ID != "" && p.Turn.ID != started.Turn.ID {
				continue
			}
			if p.Turn.Status != "completed" {
				return fmt.Errorf("%w (%s)", errTurnFailed, p.Turn.Status)
			}
			return nil
		case <-s.done:
			return fmt.Errorf("%w: codex app-server exited mid-turn", errTurnFailed)
		case <-ctx.Done():
			ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.call(ictx, "turn/interrupt", map[string]any{"threadId": t.Thread, "turnId": started.Turn.ID})
			cancel()
			return fmt.Errorf("%w: %v", errTurnFailed, ctx.Err())
		}
	}
}
