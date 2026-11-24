package connector

// The session's side of redeem by call (callredeem.go): registration and
// the claim's warm path, both in the session's waiter (one per Claude Code
// process, CLAUDE_PID, alive while the session is), never in a hook Claude
// Code waits on.
//
//   - Registration. When the waiter starts (the session starts, resumes, or
//     a turn ends), it makes the session's key if there's none and
//     registers it for the conversation with the plugin's issuer, in the
//     background (POST /v1/connector/register, signed by the key): from
//     then on the core takes claims for the conversation only by that key.
//     It also opens the waiter's connection to the issuer.
//   - The warm path. A PreToolUse hook is a new process for every call: on
//     its own, its claim opens a new TCP and TLS connection to the issuer,
//     two or three round trips before the request's own. Instead it hands
//     "call X of conversation S is mine" over a Unix socket
//     (claimd/<hash of CLAUDE_PID>.sock, 0600, in a 0700 directory) to the
//     waiter, which signs the claim with the session's key and posts it on
//     the connection it keeps open: the waiter polls the issuer already,
//     with the same transport, so over HTTP/2 the claim rides the poll's
//     connection; over HTTP/1.1 the waiter keeps one of its own warm
//     (claimWarmEvery, while the session is active).
//
// The relay posts only to the plugin's own issuer (claimIssuer, never one
// a request names) and only claims. Anything on this computer that can
// reach the socket could read the session's key file anyway.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wakeshift/connectors/internal/connectorversion"
	"github.com/wakeshift/connectors/internal/hookproof"
)

// claimWarmEvery: how often an idle relay touches the issuer (GET
// /health) to keep its connection open, for at most claimWarmFor after the
// waiter starts or the last claim.
const (
	claimWarmEvery = 45 * time.Second
	claimWarmFor   = 15 * time.Minute
)

// ClaimSocket is the claim relay's socket for a Claude Code process.
func ClaimSocket(dir, process string) string {
	return filepath.Join(dir, "claimd", short("wakeshift-claimd", process)[:16]+".sock")
}

// relayRequest is what a PreToolUse hook hands the relay: the call, its
// conversation, the app its server is pinned to ("" if none), and the
// conversation's transcript (for a key the relay makes).
type relayRequest struct {
	Call       string `json:"call"`
	Thread     string `json:"thread"`
	App        string `json:"app,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	// Async: answer 202 once the request is read, and send the claim
	// after: the default (WAKESHIFT_CLAIM_SYNC=1 waits instead).
	Async bool `json:"async,omitempty"`
	// Own: the call carries this connector's own mark (a conflict then
	// gets NoteCallClaimTaken).
	Own bool `json:"own,omitempty"`
}

type relayed struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
	Skew   string          `json:"skew,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// signedClaim is a claim's body, signed by key.
func signedClaim(key ed25519.PrivateKey, issuer, call, thread, app string) []byte {
	p := hookproof.Sign(key, hookproof.Claim, issuer, call, thread, time.Now())
	b, _ := json.Marshal(map[string]any{"call": call, "thread": thread, "app": app, "key": p.Key, "sig": p.Sig, "ts": p.TS})
	return b
}

// sendClaimWarm hands a claim to the session's relay, if one is running.
func sendClaimWarm(ctx context.Context, dir, process string, req []byte) (int, []byte, string, error) {
	if process == "" || !waiterKeyRE.MatchString(process) {
		return 0, nil, "", errors.New("no process")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", ClaimSocket(dir, process))
	if err != nil {
		return 0, nil, "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(append(append([]byte(nil), req...), '\n')); err != nil {
		return 0, nil, "", err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return 0, nil, "", err
	}
	var r relayed
	if json.Unmarshal(line, &r) != nil || r.Error != "" || r.Status == 0 {
		return 0, nil, "", errors.New("the relay couldn't send it")
	}
	return r.Status, r.Body, r.Skew, nil
}

// registerKey registers the conversation's key with issuer.
func registerKey(ctx context.Context, hc *http.Client, harness, issuer string, key ed25519.PrivateKey, thread string) (bool, error) {
	p := hookproof.Sign(key, hookproof.Register, issuer, "", thread, time.Now())
	b, _ := json.Marshal(map[string]any{"thread": thread, "key": p.Key, "sig": p.Sig, "ts": p.TS})
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(issuer, "/")+"/v1/connector/register", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectorversion.Header, ClientHeader(harness))
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var out struct {
		Registered bool `json:"registered"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return false, errors.New(resp.Status)
	}
	return out.Registered, nil
}

// serveClaims registers the waiter's conversation and runs the claim relay
// for this waiter until ctx ends or the returned func is called.
// conversation and transcript: the waiter's session's. A newer waiter of
// the same process takes the socket over.
func (h *Hook) serveClaims(ctx context.Context, conversation, transcript string) func() {
	if !RedeemByCallEnabled() || h.Harness != claudeName || h.WaiterKey == "" || !waiterKeyRE.MatchString(h.WaiterKey) {
		return func() {}
	}
	issuer := h.claimIssuer()
	if issuer == "" {
		return func() {}
	}
	dir := h.Verifier.Settings.Dir
	ctx, cancel := context.WithCancel(ctx)
	hc := client(h.HTTP) // the poller's transport: over HTTP/2, the same connection
	active := make(chan struct{}, 1)
	go func() {
		// Registration, in the background: it also opens the connection.
		start := time.Now()
		if key, err := sessionKey(dir, conversation, true, transcript); err == nil && conversation != "" {
			rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
			ok, err := registerKey(rctx, hc, h.Harness, issuer, key, conversation)
			rcancel()
			switch {
			case err != nil:
				h.logRedeem(conversation, "register", "unreachable", start)
			case ok:
				h.logRedeem(conversation, "register", "registered", start)
			default:
				h.logRedeem(conversation, "register", "conflict", start)
			}
		}
		last := time.Now()
		t := time.NewTicker(claimWarmEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-active:
				last = time.Now()
			case <-t.C:
				if time.Since(last) < claimWarmFor {
					req, _ := http.NewRequestWithContext(ctx, "GET", issuer+"/health", nil)
					if resp, err := hc.Do(req); err == nil {
						resp.Body.Close()
					}
				}
			}
		}
	}()
	path := ClaimSocket(dir, h.WaiterKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return cancel
	}
	// An older waiter's socket: this one takes over. A Unix socket is
	// reached by its path, so the older listener gets no connection after
	// this, and every request names its own conversation (the relay signs
	// with that conversation's key, whichever waiter it is).
	// Under a lock, so two waiters starting at once can't remove each
	// other's new socket between the remove and the listen.
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return cancel
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err == nil {
		os.Chmod(path, 0o600)
	}
	unlock()
	if err != nil {
		return cancel
	}
	// Closing doesn't unlink the path: by then it may be a newer waiter's
	// socket. A path left behind refuses connections at once (the hook
	// then sends the claim itself), and the next waiter replaces it.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(claimTimeout() + time.Second))
				line, err := bufio.NewReader(io.LimitReader(conn, 1<<16)).ReadBytes('\n')
				var in relayRequest
				if err != nil || json.Unmarshal(line, &in) != nil || in.Call == "" || len(in.Call) > 200 || in.Thread == "" || len(in.Thread) > 200 {
					return
				}
				select {
				case active <- struct{}{}:
				default:
				}
				if in.Async {
					conn.Write([]byte(`{"status":202}` + "\n"))
					conn.Close()
				}
				var r relayed
				key, err := sessionKey(dir, in.Thread, true, in.Transcript)
				if err != nil {
					r.Error = "no_key"
				} else {
					rctx, rcancel := context.WithTimeout(ctx, claimTimeout())
					status, body, skew, err := sendClaim(rctx, hc, h.Harness, issuer, signedClaim(key, issuer, in.Call, in.Thread, in.App))
					rcancel()
					if err != nil {
						r.Error = "unreachable"
					} else {
						r.Status, r.Skew = status, skew
						if json.Valid(body) {
							r.Body = body
						}
					}
				}
				if in.Async {
					// Answered already; the claim's send didn't depend on
					// the connection. The hook's not there to read the
					// answer, so a claim taken is noted here.
					h.noteClaimTaken(in.Thread, in.Own, r.Status, r.Body)
					return
				}
				out, _ := json.Marshal(r)
				conn.Write(append(out, '\n'))
			}()
		}
	}()
	return func() {
		cancel()
		ln.Close()
	}
}
