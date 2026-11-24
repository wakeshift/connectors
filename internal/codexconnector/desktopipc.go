package codexconnector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// The Codex desktop app's IPC: its windows (and Codex's TUI, for /ide)
// talk through a router on a Unix socket.
//
// The transport here (socket paths, the socket and peer checks, the
// length-prefixed frames, the request envelope, sending without
// registering, and answering what the router asks: discovery with
// canHandle false, any request with no-handler-for-request) follows the
// Apache-2.0 client OpenAI publishes in openai/codex,
// codex-rs/tui/src/ide_context/ipc.rs. The two thread methods and their
// versions (thread-owner-discovery v1, thread-follower-start-turn v2) are
// observed; not a published interface, so a version or shape the
// connector doesn't expect turns IPC off for the thread (no guessing).
//
// The connector never registers with the router (no initialize), so it's
// never sent other clients' broadcasts, and it never follows a thread, which
// would stream the conversation to it. It asks about, and starts turns on,
// only the thread it was started for.

const (
	ipcOwnerDiscovery        = "thread-owner-discovery"
	ipcOwnerDiscoveryVersion = 1
	ipcStartTurn             = "thread-follower-start-turn"
	ipcStartTurnVersion      = 2
	ipcHostLocal             = "local" // the app's own machine (hostId)
	ipcSourceClient          = "wakeshift-connector"
	ipcMaxFrame              = 256 << 20 // the router's limit
	ipcMaxSocketPath         = 103       // sun_path on macOS, less the NUL
	ipcRequestTimeout        = 10 * time.Second
	ipcStartTurnTimeout      = 30 * time.Second
)

// errAppClosed: no router answers on any of the sockets (the app isn't
// running).
var errAppClosed = errors.New("the Codex desktop app isn't running")

// errNoOwner: the app runs, but no window has the thread loaded.
var errNoOwner = errors.New("no Codex desktop window has the thread")

// errIPCUnsupported: the app answered in a way this connector doesn't
// know (a version mismatch, an unexpected shape): IPC is off for the
// thread.
type errIPCUnsupported struct{ why string }

func (e *errIPCUnsupported) Error() string { return "codex desktop IPC: " + e.why }

// ipcError is an error code the router or a window answered with.
type ipcError struct{ Code string }

func (e *ipcError) Error() string { return "codex desktop: " + e.Code }

// DesktopSockets are where the app's router listens, in the order Codex's
// own client tries them: CODEX_HOME/ipc/ipc.sock (or, when that path is
// too long for a socket, the app's link to it under /tmp), then the legacy
// socket in the temp directory.
func DesktopSockets() []string {
	dir := filepath.Join(CodexHome(), "ipc")
	primary := filepath.Join(dir, "ipc.sock")
	uid := os.Getuid()
	if len(primary) > ipcMaxSocketPath {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			real = dir
		}
		sum := sha256.Sum256([]byte(real))
		primary = filepath.Join("/tmp", "codex-ipc-"+strconv.Itoa(uid), hex.EncodeToString(sum[:]), "ipc.sock")
	}
	legacy := filepath.Join(os.TempDir(), "codex-ipc", "ipc-"+strconv.Itoa(uid)+".sock")
	if uid == 0 {
		return []string{primary, filepath.Join(os.TempDir(), "codex-ipc", "ipc.sock"), filepath.Join(os.TempDir(), "codex-ipc", "ipc-0.sock")}
	}
	return []string{primary, legacy}
}

// checkDesktopSocket refuses a socket another user could have put there or
// could reach: it must be this user's socket, readable and writable by
// this user alone (0600), in a directory that's this user's and that
// nobody else can write to.
func checkDesktopSocket(path string) error {
	uid := os.Getuid()
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !dir.IsDir() || ownerOf(dir) != uid {
		return errors.New("the socket's directory isn't this user's")
	}
	if dir.Mode().Perm()&0o022 != 0 {
		return errors.New("the socket's directory is writable by other users")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket == 0 || ownerOf(st) != uid {
		return errors.New("the socket isn't this user's")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return errors.New("the socket is open to other users (not 0600)")
	}
	return nil
}

func ownerOf(fi os.FileInfo) int {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid)
	}
	return -1
}

// ipcConn is one connection to the router, for one wake.
type ipcConn struct{ c net.Conn }

// dialDesktop connects to the first socket that's there, checked
// (checkDesktopSocket, and the peer's user), like Codex's client: a later
// socket is tried only if an earlier one can't be connected to.
func dialDesktop(ctx context.Context, sockets []string) (*ipcConn, error) {
	var last error = errAppClosed
	for _, path := range sockets {
		if err := checkDesktopSocket(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("codex desktop: %s: %w", path, err)
			}
			continue
		}
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
				continue // a socket left by an app that quit
			}
			last = err
			continue
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			c.Close()
			return nil, errors.New("codex desktop: not a Unix socket")
		}
		if err := checkPeer(uc); err != nil {
			c.Close()
			return nil, fmt.Errorf("codex desktop: %w", err)
		}
		return &ipcConn{c: c}, nil
	}
	return nil, last
}

func (c *ipcConn) close() { c.c.Close() }

func (c *ipcConn) write(m any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > ipcMaxFrame {
		return errors.New("codex desktop: message too large")
	}
	frame := make([]byte, 4+len(b))
	binary.LittleEndian.PutUint32(frame, uint32(len(b)))
	copy(frame[4:], b)
	_, err = c.c.Write(frame)
	return err
}

func (c *ipcConn) read() (map[string]json.RawMessage, error) {
	var head [4]byte
	if _, err := io.ReadFull(c.c, head[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(head[:])
	if n > ipcMaxFrame {
		return nil, errors.New("codex desktop: frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c.c, b); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("codex desktop: a frame that isn't JSON: %w", err)
	}
	return m, nil
}

func str(m map[string]json.RawMessage, k string) string {
	var s string
	json.Unmarshal(m[k], &s)
	return s
}

func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// request sends one request and waits for its response, answering what
// the router asks meanwhile: discovery with canHandle false, any request
// with no-handler-for-request, at once; broadcasts are ignored. An error
// response is an *ipcError.
func (c *ipcConn) request(ctx context.Context, method string, version int, target string, timeout time.Duration, params any) (map[string]json.RawMessage, error) {
	deadline := time.Now().Add(timeout + 5*time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.c.SetDeadline(time.Now()) })
	defer stop()
	id := newRequestID()
	req := map[string]any{"type": "request", "requestId": id, "sourceClientId": ipcSourceClient,
		"version": version, "method": method, "params": params, "timeoutMs": timeout.Milliseconds()}
	if target != "" {
		req["targetClientId"] = target
	}
	if err := c.write(req); err != nil {
		return nil, err
	}
	for {
		m, err := c.read()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch str(m, "type") {
		case "response":
			if str(m, "requestId") != id {
				continue
			}
			switch str(m, "resultType") {
			case "success":
				return m, nil
			case "error":
				return nil, &ipcError{Code: str(m, "error")}
			}
			return nil, &errIPCUnsupported{"a response that's neither success nor error"}
		case "broadcast", "client-discovery-response":
		case "client-discovery-request":
			if err := c.write(map[string]any{"type": "client-discovery-response", "requestId": str(m, "requestId"),
				"response": map[string]bool{"canHandle": false}}); err != nil {
				return nil, err
			}
		case "request":
			if err := c.write(map[string]any{"type": "response", "requestId": str(m, "requestId"),
				"resultType": "error", "error": "no-handler-for-request"}); err != nil {
				return nil, err
			}
		default:
			return nil, &errIPCUnsupported{fmt.Sprintf("a message of type %q", str(m, "type"))}
		}
	}
}

// unsupported turns the codes that say this connector and the app don't
// agree on a method into errIPCUnsupported.
func unsupported(method string, err error) error {
	var ie *ipcError
	if errors.As(err, &ie) && (ie.Code == "request-version-mismatch" || ie.Code == "no-handler-for-request" || ie.Code == "thread-follower-response-method-mismatch") {
		return &errIPCUnsupported{method + ": " + ie.Code}
	}
	return err
}

// owner finds the window that has thread loaded: its client ID, or
// errNoOwner.
func (c *ipcConn) owner(ctx context.Context, thread string) (string, error) {
	r, err := c.request(ctx, ipcOwnerDiscovery, ipcOwnerDiscoveryVersion, "", ipcRequestTimeout,
		map[string]any{"hostId": ipcHostLocal, "conversationId": thread})
	var ie *ipcError
	if errors.As(err, &ie) && ie.Code == "no-client-found" {
		return "", errNoOwner
	}
	if err != nil {
		return "", unsupported(ipcOwnerDiscovery, err)
	}
	owner := str(r, "handledByClientId")
	var result struct {
		SupportsUntrustedAppInput *bool `json:"supportsUntrustedAppInput"`
	}
	if owner == "" || json.Unmarshal(r["result"], &result) != nil || result.SupportsUntrustedAppInput == nil {
		return "", &errIPCUnsupported{ipcOwnerDiscovery + ": an answer of an unknown shape"}
	}
	return owner, nil
}

// startTurn asks the owning window to start a turn on thread with output
// as a tool output (outside data, never the user's words): the same
// turn/start the connector sends Codex's daemon, which the window runs in
// its own conversation, so it shows there live. It returns the turn's ID;
// a success of an unknown shape is errIPCUnsupported with started set (the
// window took it).
func (c *ipcConn) startTurn(ctx context.Context, owner, thread, output string) (string, error) {
	r, err := c.request(ctx, ipcStartTurn, ipcStartTurnVersion, owner, ipcStartTurnTimeout,
		map[string]any{"conversationId": thread, "turnStart": map[string]any{"request": map[string]any{
			"threadId": thread, "input": []any{}, "toolOutput": map[string]any{"name": "wakeshift_event", "output": output}}}})
	if err != nil {
		return "", unsupported(ipcStartTurn, err)
	}
	var out struct {
		Result struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		} `json:"result"`
	}
	if json.Unmarshal(r["result"], &out) != nil || out.Result.Turn.ID == "" {
		return "", &errStartedUnknown{}
	}
	return out.Result.Turn.ID, nil
}

// errStartedUnknown: the window accepted the turn, but its answer had an
// unknown shape.
type errStartedUnknown struct{}

func (*errStartedUnknown) Error() string {
	return "codex desktop IPC: " + ipcStartTurn + ": accepted, with an answer of an unknown shape"
}
