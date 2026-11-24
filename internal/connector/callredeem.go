package connector

// Redeem by call for Claude Code, on
// with WAKESHIFT_REDEEM_BY_TOOL_CALL=1 in the hook's environment (and the
// core's flag of the same name; off by default). The core's side:
// internal/appapi (callredeem.go).
//
// Claude Code's PostToolUse hook gets the call's ID (tool_use_id) at once,
// but the result's _meta (the beacon) only from the transcript, which
// Claude Code writes up to seconds later. With the flag:
//
//   - PreToolUse (synchronous, --event claim): for a call whose harness
//     argument carries a Wakeshift plugin mark, claim the call for this
//     Claude Code session's key at the plugin's issuer, before the app's
//     server sees the ID (claimCall). Nothing for unmarked calls, so no
//     other server's calls leave the machine; nothing while this hook holds
//     a link for the conversation and the server's app, unless the mark
//     isn't this plugin's own (someone had the model pass another), which
//     is claimed whatever is held, but only for a server pinned to an app.
//     Never blocks or fails the call: the request gives up after
//     claimTimeout, and the hook prints nothing.
//   - PostToolUse: a claimed call is redeemed by its ID, signed by the
//     same key (callFirst). Anything else, or a refusal, falls back to the
//     beacon from the transcript, as before, and the beacon's redeem says
//     why (X-Wakeshift-Call-Redeem).
//   - SubagentStart (--event subagent-context): the mark, for the
//     subagent's own context, as SessionStart's is for the session's.
//
// The key is per Claude Code session (sessions/<hash>/key, 0600), made on
// the first claim, never at SessionStart (which stays network-free and
// never waits on this); a waiter deletes the keys of sessions whose
// transcript is gone or idle for 30 days (SweepSessionKeys). Every claim
// and redeem is logged, without IDs, to logs/redeems.log; `wakeshift
// doctor` summarizes it (Doctor).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/connectors/internal/connectorversion"
	"github.com/wakeshift/connectors/internal/hookproof"
	"github.com/wakeshift/connectors/internal/pluginmark"
	"github.com/wakeshift/connectors/internal/version"
)

// RedeemByCallEnabled: the flag, WAKESHIFT_REDEEM_BY_TOOL_CALL=1.
func RedeemByCallEnabled() bool { return os.Getenv("WAKESHIFT_REDEEM_BY_TOOL_CALL") == "1" }

const (
	// ClaudeClaimEvent is the --event of the plugin's PreToolUse hook.
	ClaudeClaimEvent = "claim"
	// ClaudeSubagentContextEvent is the --event of its SubagentStart hook.
	ClaudeSubagentContextEvent = "subagent-context"
)

// claimTimeout bounds the claim's request; past it the call goes on
// unclaimed (the beacon connects it). WAKESHIFT_CLAIM_TIMEOUT_MS sets it.
func claimTimeout() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("WAKESHIFT_CLAIM_TIMEOUT_MS")); err == nil && ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 750 * time.Millisecond
}

// Notes for the agent (and through it the user), saved for the next
// prompt or wake (Claude Code shows an async hook's context only then).
const (
	// NoteCallKeyMismatch: someone else redeemed, or tried to, this
	// conversation's call with another key (refused).
	NoteCallKeyMismatch = "Wakeshift: someone else tried to connect this conversation with %d other key(s) using one of its tool calls' IDs, and was refused. This conversation is connected, and nothing was delivered elsewhere. If it keeps happening, tell the user: something that sees this conversation's tool calls (the app's server, a proxy, a log) may be leaking them."
	// NoteCallClaimTaken: another key claimed a call before this session
	// did. The claim is async and races the call, so it's someone who saw
	// the call's ID (on this computer, or on its way to the app). The core
	// then binds the call to neither key, and the beacon connects it.
	NoteCallClaimTaken = "Wakeshift: another key claimed one of this conversation's tool calls first, so that call can't connect the conversation by its ID; its result may still connect it. If it keeps happening, tell the user: something that sees this conversation's tool calls (the app's server, a proxy, a log, or another program on this computer) may be using their IDs."
)

// pinnedApp is the app the server is pinned to, "" if none (or unreadable).
func (p Pins) pinnedApp(server string) string {
	f, err := p.load()
	if err != nil {
		return ""
	}
	return f.Servers[server]
}

// claimIssuer is where claims go: the plugin's own issuer (its
// plugin-issuer file), else the one issuer trusted here; "" if there are
// several and no plugin names one.
func (h *Hook) claimIssuer() string {
	set := h.Verifier.Settings
	if b, err := os.ReadFile(filepath.Join(set.Dir, PluginIssuerFile)); err == nil {
		if iss, err := CheckIssuer(strings.TrimSpace(string(b))); err == nil {
			for _, t := range set.Issuers {
				if t == iss {
					return iss
				}
			}
		}
	}
	if len(set.Issuers) == 1 {
		return set.Issuers[0]
	}
	return ""
}

// claimRecord is what a claim left for the call's PostToolUse hooks
// (claims/<hash of the call>). Outcome "redeemed": a PostToolUse hook
// connected the call by its ID already (the plugin's and the installer's
// hooks can both run).
type claimRecord struct {
	Issuer  string `json:"issuer"`
	App     string `json:"app,omitempty"`
	Outcome string `json:"outcome"` // claimed, timeout…, redeemed, or why not
	At      int64  `json:"at"`
}

func (h *Hook) claimPath(call string) string {
	return filepath.Join(h.Verifier.Settings.Dir, "claims", short("wakeshift-call", call))
}

// claimRank orders a claim record's outcomes: a hook never writes over a
// better one (the plugin's and the installer's hooks can both run for a
// call, and one's timeout mustn't hide the other's claim).
func claimRank(outcome string) int {
	switch {
	case outcome == "redeemed":
		return 3
	case outcome == "claimed":
		return 2
	case outcome == "handed" || strings.HasPrefix(outcome, "timeout"):
		// Handed over (or timed out): the claim may be there, or not yet.
		return 1
	}
	return 0
}

func (h *Hook) saveClaim(call string, rec claimRecord) {
	p := h.claimPath(call)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	unlock, err := lockFile(p + ".lock")
	if err != nil {
		return
	}
	defer unlock()
	if b, err := os.ReadFile(p); err == nil {
		var old claimRecord
		if json.Unmarshal(b, &old) == nil && claimRank(old.Outcome) > claimRank(rec.Outcome) {
			return
		}
	}
	if bb, err := json.Marshal(rec); err == nil {
		os.WriteFile(p, bb, 0o600)
	}
}

type claudePreInput struct {
	SessionID      string                     `json:"session_id"`
	TranscriptPath string                     `json:"transcript_path"`
	ToolName       string                     `json:"tool_name"`
	ToolUseID      string                     `json:"tool_use_id"`
	AgentID        string                     `json:"agent_id"`
	ToolInput      map[string]json.RawMessage `json:"tool_input"`
	MCPServer      struct {
		Name string `json:"name"`
	} `json:"mcp_server"`
}

// ownMark: arg is the mark this connector gives (the plugin's, or the
// installer's hooks').
func (h *Hook) ownMark(arg string) bool {
	set := h.Verifier.Settings
	return arg == pluginmark.Value(h.Harness, version.Version, set.Issuers) || arg == pluginmark.HooksValue(h.Harness, version.Version, set.Issuers)
}

// claimCall is the PreToolUse hook (the package comment). It never fails
// the call and prints nothing.
func (h *Hook) claimCall(ctx context.Context, b []byte) {
	if !RedeemByCallEnabled() {
		return
	}
	start := time.Now()
	var c claudePreInput
	if json.Unmarshal(b, &c) != nil || c.SessionID == "" || c.ToolUseID == "" || !strings.HasPrefix(c.ToolName, "mcp__") {
		return
	}
	var arg string
	json.Unmarshal(c.ToolInput["harness"], &arg)
	if _, _, ok := pluginmark.Split(arg); !ok {
		return // unmarked: nothing leaves the machine, nothing logged
	}
	own := h.ownMark(arg)
	server := c.MCPServer.Name
	if server == "" {
		server = MCPServerOf(c.ToolName)
	}
	app := h.pins().pinnedApp(server)
	rec := claimRecord{App: app, At: start.Unix()}
	done := func(outcome string) {
		// The record keeps the outcome alone; the log, how it went too
		// (" via=warm|direct", " skew=…").
		rec.Outcome = strings.Fields(outcome)[0]
		h.saveClaim(c.ToolUseID, rec)
		mark := "own"
		if !own {
			mark = "foreign"
		}
		note := ""
		if c.AgentID != "" {
			note = " subagent=1"
		}
		h.logRedeem(c.SessionID, "claim", outcome+" mark="+mark+note, start)
	}
	if app != "" && own && h.Links.LoadFor(c.SessionID, app) != "" {
		done("skipped_held")
		return
	}
	// This plugin's own mark to a server no app is pinned to yet is claimed:
	// it's the server's first sight (the first call of an app the model
	// passed the mark to, which only a Wakeshift app's tool takes, since the
	// mark is the harness argument Wakeshift's SDKs define), and the redeem
	// pins the app the core answers. Only calls the model marked are sent,
	// never another server's unmarked ones.
	if !own && app == "" {
		// A mark this plugin didn't give, to a server not pinned to an
		// app: the model can be had to pass one to any server, so nothing
		// about that call is sent.
		done("skipped_foreign_unpinned")
		return
	}
	if rec.Issuer = h.claimIssuer(); rec.Issuer == "" {
		done("no_issuer")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, claimTimeout())
	defer cancel()
	// The claim is async: the hook hands it over and returns, so the call
	// isn't held up, and the claim races the call to the core ("handed").
	// It nearly always gets there first (it leaves first, and the app's
	// server has the call to handle before it subscribes); a call whose
	// claim lost connects from the beacon, as before. First choice: the
	// session's waiter, which signs it with the session's key (registered
	// when the session started) and sends it on a connection it holds open
	// (claimd.go). Without a waiter: a detached connector process
	// (sendClaimDetached). WAKESHIFT_CLAIM_SYNC=1 (tests, measurements)
	// waits for the core's answer instead, within claimTimeout.
	async := os.Getenv("WAKESHIFT_CLAIM_SYNC") != "1"
	via := "warm"
	req, _ := json.Marshal(relayRequest{Call: c.ToolUseID, Thread: c.SessionID, App: app, Transcript: c.TranscriptPath, Async: async, Own: own})
	status, rb, skew, err := sendClaimWarm(cctx, h.Verifier.Settings.Dir, os.Getenv("CLAUDE_PID"), req)
	if err == nil && async && status == http.StatusAccepted {
		done("handed via=warm")
		return
	}
	if err != nil && cctx.Err() == nil {
		via = "direct"
		key, kerr := sessionKey(h.Verifier.Settings.Dir, c.SessionID, true, c.TranscriptPath)
		if kerr != nil {
			done("no_key")
			return
		}
		body := signedClaim(key, rec.Issuer, c.ToolUseID, c.SessionID, app)
		if async && h.Bin != "" && h.startClaimSender(c.ToolUseID, rec, body, own) == nil {
			done("handed via=detached")
			return
		}
		status, rb, skew, err = sendClaim(cctx, h.HTTP, h.Harness, rec.Issuer, body)
	}
	if err != nil {
		if cctx.Err() != nil {
			done("timeout via=" + via)
		} else {
			done("unreachable via=" + via)
		}
		return
	}
	var out struct {
		Claimed bool `json:"claimed"`
	}
	json.Unmarshal(rb, &out)
	switch {
	case status == http.StatusOK && out.Claimed:
		done("claimed via=" + via)
	case status == http.StatusOK:
		done("conflict via=" + via)
		h.noteClaimTaken(c.SessionID, own, status, rb)
	case status == http.StatusUnauthorized:
		done("bad_proof" + skew)
	default:
		done(fmt.Sprintf("refused_%d", status))
	}
}

// sendClaim posts a claim to issuer: the answer's status, body (at most
// 64 KiB) and, on a refused proof, the clock's skew.
func sendClaim(ctx context.Context, hc *http.Client, harness, issuer string, body []byte) (int, []byte, string, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", strings.TrimSuffix(issuer, "/")+"/v1/connector/claim", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectorversion.Header, ClientHeader(harness))
	resp, err := client(hc).Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	skew := ""
	if resp.StatusCode == http.StatusUnauthorized {
		skew = skewNote(resp)
	}
	return resp.StatusCode, rb, skew, nil
}

// skewNote: the core's time on a refused proof, as this clock's skew.
func skewNote(resp *http.Response) string {
	if t, err := strconv.ParseInt(resp.Header.Get("X-Wakeshift-Time"), 10, 64); err == nil {
		return fmt.Sprintf(" skew=%ds", time.Now().Unix()-t)
	}
	return ""
}

type fallbackKey struct{}

// withFallback carries why a redeem by call wasn't made (or was refused)
// to the beacon's redeem (redeemHeld), which sends it to the core.
func withFallback(ctx context.Context, why string) context.Context {
	return context.WithValue(ctx, fallbackKey{}, why)
}

func fallbackOf(ctx context.Context) string {
	s, _ := ctx.Value(fallbackKey{}).(string)
	return s
}

// byCall is what PostToolUse's call-first step made of a result
// (callFirst): done, the call was handled by its ID (note, if any, for the
// agent: "" when another hook process connected it already); else the
// result as read from the transcript, and the context its beacon's redeem
// carries the reason in.
type byCall struct {
	done bool
	note string
	in   Input
	ctx  context.Context
}

// callFirst is PostToolUse's first step for a result whose _meta comes
// late (Input.lateMeta): redeem a claimed call by its ID; else read the
// result's _meta from the transcript for the beacon, as before.
func (h *Hook) callFirst(ctx context.Context, in Input) byCall {
	note, why := h.connectByCall(ctx, in)
	if why == "" {
		return byCall{done: true, note: note, in: in, ctx: ctx}
	}
	if m := in.lateMeta(); m != nil {
		in.ToolResult = m
	}
	return byCall{in: in, ctx: withFallback(ctx, why)}
}

// connectByCall redeems a claimed call's ticket by its ID and keeps the
// link: the note (as connectVerified's) and "" if it was handled (by this
// process, or by another hook process before it), else "" and why not.
func (h *Hook) connectByCall(ctx context.Context, in Input) (string, string) {
	start := time.Now()
	if in.ToolCallID == "" || in.Server == "" || in.MCPTool == "" || h.aliased() {
		return "", "no_call"
	}
	// Read and redeemed under the redeems' lock: the plugin's and the
	// installer's hooks may both run for the call, and only one redeems.
	unlock := h.Links.LockRedeems()
	defer unlock()
	b, err := os.ReadFile(h.claimPath(in.ToolCallID))
	if err != nil {
		return "", "no_claim"
	}
	var rec claimRecord
	if json.Unmarshal(b, &rec) != nil {
		return "", "no_claim"
	}
	switch {
	case rec.Outcome == "redeemed":
		return "", "" // another hook process connected it
	case rec.Outcome != "claimed" && rec.Outcome != "handed" && !strings.HasPrefix(rec.Outcome, "timeout"):
		// A claim that timed out may have been stored all the same: tried.
		return "", "claim_" + strings.Fields(rec.Outcome + " none")[0]
	}
	key, err := sessionKey(h.Verifier.Settings.Dir, in.Conversation, false, "")
	if err != nil {
		h.logRedeem(in.Conversation, "call", "no_key", start)
		return "", "no_key"
	}
	held := ""
	if rec.App != "" {
		held = h.Links.LoadFor(in.Conversation, rec.App)
	}
	// No retry: the core binds a ticket to a claim when it mints the
	// ticket, during the call, so a claim that wasn't there by then (it
	// lost the race, or timed out) never binds it; the beacon connects it.
	res, err := redeemCallAt(ctx, h.HTTP, h.Harness, rec.Issuer, key, in, rec.App, held, h.WaitCommand)
	if err != nil {
		why := "refused"
		if rec.Outcome == "handed" {
			why = "lost_race" // most likely: a handed claim that got there after the ticket was minted
		}
		if errors.Is(err, ErrUnreachable) {
			why = "unreachable"
		}
		h.logRedeem(in.Conversation, "call", why, start)
		return "", why
	}
	app := rec.App
	if app == "" {
		// The server's first sight: pinned to the app the core answered,
		// as a beacon's app is pinned (trust on first use).
		if res.app.ID == "" || h.pins().Allow(in.Server, res.app.ID) != nil {
			h.logRedeem(in.Conversation, "call", "pin_refused", start)
			return "", "pin_refused"
		}
		app = res.app.ID
	}
	// The ticket is spent either way, so the call is marked redeemed (a
	// second hook set doesn't try it again), whatever the save does.
	rec.Outcome = "redeemed"
	h.saveClaim(in.ToolCallID, rec)
	if err := h.Links.SaveFor(in.Conversation, res.url, app); err != nil {
		// As for a beacon (connectVerified): the agent is told, and the
		// conversation connects from the app's next result, whose beacon
		// is fresh (the core issues one while the session isn't connected).
		h.logRedeem(in.Conversation, "call", "link_not_saved", start)
		return fmt.Sprintf(NoteFailed, "its link couldn't be saved"), ""
	}
	h.logRedeem(in.Conversation, "call", "ok", start)
	note := fmt.Sprintf(NoteConnected, h.When)
	if t := h.toolsOnConnect(in, res.app, held == ""); t != "" {
		note += "\n\n" + t
	}
	if res.refused > 0 {
		note += "\n\n" + fmt.Sprintf(NoteCallKeyMismatch, res.refused)
		h.logRedeem(in.Conversation, "call", fmt.Sprintf("refused_attempts=%d", res.refused), start)
	}
	return note, ""
}

type callRedeemed struct {
	url     string
	app     RedeemedApp
	refused int
}

// redeemCallAt is one redeem by call at issuer.
func redeemCallAt(ctx context.Context, hc *http.Client, harness, issuer string, key ed25519.PrivateKey, in Input, app, held, wait string) (callRedeemed, error) {
	p := hookproof.Sign(key, hookproof.Call, issuer, in.ToolCallID, in.Conversation, time.Now())
	body := map[string]any{"call": in.ToolCallID, "thread": in.Conversation, "tool": in.MCPTool, "app": app, "key": p.Key, "sig": p.Sig, "ts": p.TS}
	redeem := strings.TrimSuffix(issuer, "/") + "/v1/connector/redeem-call"
	// Only a link for this app, to its own issuer.
	if held != "" && app != "" && linkOnOrigin(held, redeem) {
		body["held"] = held
	}
	if wait != "" {
		body["wait"] = wait
	}
	b, _ := json.Marshal(body)
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, "POST", redeem, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(connectorversion.Header, ClientHeader(harness))
	resp, err := client(hc).Do(req)
	if err != nil {
		return callRedeemed{}, ErrUnreachable
	}
	defer resp.Body.Close()
	var out struct {
		URL     string       `json:"url"`
		App     *RedeemedApp `json:"app"`
		Refused int          `json:"refused_attempts"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.URL == "" {
		return callRedeemed{}, fmt.Errorf("connector: redeem by call refused (%d)", resp.StatusCode)
	}
	if err := CheckStreamLink(out.URL, []string{issuer}); err != nil {
		return callRedeemed{}, err
	}
	var ra RedeemedApp
	if out.App != nil && (app == "" || out.App.ID == app) {
		ra = *out.App
	}
	if app != "" && ra.ID != app {
		return callRedeemed{}, errors.New("connector: redeem by call answered for another app")
	}
	return callRedeemed{url: out.URL, app: ra, refused: out.Refused}, nil
}

// RedeemLog is the redeem log's path for a hook's state directory.
func RedeemLog(dir string) string { return filepath.Join(dir, "logs", "redeems.log") }

// logRedeem writes one line to logs/redeems.log (as waiters.log: the
// directory 0700, the file 0600, rotated at waiterLogMax): when, a short
// hash of the conversation, the path (claim, call, beacon), the outcome and
// how long it took. No IDs, links, keys or URLs.
func (h *Hook) logRedeem(conversation, path, outcome string, start time.Time) {
	if h.Verifier == nil || h.Verifier.Settings.Dir == "" || !RedeemByCallEnabled() {
		return
	}
	AppendRedeemLog(h.Verifier.Settings.Dir, conversation, path, outcome, start)
}

// AppendRedeemLog writes logRedeem's line to dir's logs/redeems.log, for
// hooks outside this package (Codex's). outcome must hold no IDs, links,
// keys or URLs.
func AppendRedeemLog(dir, conversation, path, outcome string, start time.Time) {
	if dir == "" {
		return
	}
	sum := sha256.Sum256([]byte(conversation))
	line := fmt.Sprintf("%s conversation=%s path=%s outcome=%q ms=%d\n", time.Now().UTC().Format(time.RFC3339Nano),
		hex.EncodeToString(sum[:4]), path, outcome, time.Since(start).Milliseconds())
	p := RedeemLog(dir)
	if err := os.Mkdir(filepath.Dir(p), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return
	}
	if st, err := os.Stat(p); err == nil && st.Size() > waiterLogMax {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}

// logBeacon logs the beacon's redeem after a call that wasn't redeemed by
// its ID (none if the result had no beacon): connected or not, and why the
// call wasn't.
func (h *Hook) logBeacon(ctx context.Context, conversation, token, note string, start time.Time) {
	if token == "" {
		return
	}
	outcome := "not_connected"
	if strings.HasPrefix(note, "Wakeshift: connected.") {
		outcome = "ok"
	}
	h.logRedeem(conversation, "beacon", outcome+" fallback="+fallbackOf(ctx), start)
}

// ClaudeClaimSendEvent is the --event of the detached process that sends a
// claim the hook signed, when no waiter could (sendClaimDetached).
const ClaudeClaimSendEvent = "claim-send"

// detachedClaim is what the hook hands its detached sender on stdin.
type detachedClaim struct {
	Call string          `json:"call"`
	Rec  claimRecord     `json:"rec"`
	Body json.RawMessage `json:"body"`
	Own  bool            `json:"own,omitempty"`
}

// noteClaimTaken saves NoteCallClaimTaken for the conversation if the
// core's answer to a claim of a call with this connector's own mark says
// another key claimed it first. The async senders (the relay, the detached
// sender) call it once the answer comes, as the sync claim does.
func (h *Hook) noteClaimTaken(conversation string, own bool, status int, body []byte) {
	var out struct {
		Claimed bool `json:"claimed"`
	}
	if !own || status != http.StatusOK || json.Unmarshal(body, &out) != nil || out.Claimed {
		return
	}
	h.saveNote(conversation, NoteCallClaimTaken)
}

// startClaimSender starts a detached connector process that sends the
// signed claim and records its outcome, so the hook returns at once.
func (h *Hook) startClaimSender(call string, rec claimRecord, body []byte, own bool) error {
	cmd := exec.Command(h.Bin, "hook", "--harness", h.Harness, "--event", ClaudeClaimSendEvent)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // as StartResolver: not the hook's process group
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	b, _ := json.Marshal(detachedClaim{Call: call, Rec: rec, Body: body, Own: own})
	stdin.Write(append(b, '\n'))
	stdin.Close()
	return cmd.Process.Release()
}

// sendClaimDetached is the detached sender (ClaudeClaimSendEvent): it
// posts the claim to the plugin's issuer (never one it's handed) and
// records the outcome for the call's PostToolUse hook.
func (h *Hook) sendClaimDetached(ctx context.Context, b []byte) {
	if !RedeemByCallEnabled() {
		return
	}
	start := time.Now()
	var d detachedClaim
	issuer := h.claimIssuer()
	if json.Unmarshal(b, &d) != nil || d.Call == "" || issuer == "" || d.Rec.Issuer != issuer || !json.Valid(d.Body) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, claimTimeout())
	defer cancel()
	status, rb, _, err := sendClaim(cctx, h.HTTP, h.Harness, issuer, d.Body)
	var out struct {
		Claimed bool `json:"claimed"`
	}
	json.Unmarshal(rb, &out)
	rec := d.Rec
	switch {
	case err != nil && cctx.Err() != nil:
		rec.Outcome = "timeout"
	case err != nil:
		rec.Outcome = "unreachable"
	case status == http.StatusOK && out.Claimed:
		rec.Outcome = "claimed"
	case status == http.StatusOK:
		rec.Outcome = "conflict"
	default:
		rec.Outcome = fmt.Sprintf("refused_%d", status)
	}
	h.saveClaim(d.Call, rec)
	var thread struct {
		Thread string `json:"thread"`
	}
	json.Unmarshal(d.Body, &thread)
	if err == nil {
		h.noteClaimTaken(thread.Thread, d.Own, status, rb)
	}
	h.logRedeem(thread.Thread, "claim", rec.Outcome+" via=detached-send", start)
}
