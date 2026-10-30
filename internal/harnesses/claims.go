package harnesses

import "strings"

// What an agent says about itself: an app's subscribe tool takes two
// optional arguments (the SDK's ws.args), the program the agent runs in
// (HarnessArg) and its own ID for the conversation (ConversationArg), so
// the core can give it the right instructions where its MCP traffic
// doesn't tell (Grok Bot's and Cursor's cloud agents' calls are Cursor's).
//
// Both are self-reported hints. A claim never grants anything: it only
// refines a harness the call was recognized as into one of its Family
// (Cursor → Grok Bot or a Cursor cloud agent), or names the harness of a
// call nothing recognized, which is no more than stating that client name
// would do (Recognize). A claim that contradicts what the call was
// recognized as is ignored (Refine).

// The tool arguments' names, as the SDK's ws.args and appkit's Args have
// them.
const (
	SessionArg      = "session_key"
	HarnessArg      = "harness"
	ConversationArg = "conversation_id"
)

// SessionDescription is what the agent is told the session argument is.
const SessionDescription = "This conversation's session_key, from an earlier result (a secret token, or a handle such as claude-code#2). Keep it to this conversation; don't share or log it."

// ArgDescription is what the agent is told the harness argument is: the
// harnesses it may name (Claim, in the order of All), and that any other
// program gives its own name.
func ArgDescription() string {
	var names []string
	for _, h := range all {
		if h.Claim == "" {
			continue
		}
		n := h.Claim
		if h.ClaimHint != "" {
			n += " (" + h.ClaimHint + ")"
		}
		names = append(names, n)
	}
	return "The program you're running in: " + strings.Join(names, ", ") + ", or your program's name if it isn't listed"
}

// ConversationDescription is what the agent is told the conversation
// argument is.
const ConversationDescription = "Your own ID for this conversation, if your program gives you one. " +
	"Cursor and Grok Bot: the output of `printenv CURSOR_CONVERSATION_ID`. " +
	"Codex and Antigravity send it automatically; others leave it empty"

// Claimed is the harness a claim names (case and spacing aside): its ID,
// Claim, an alias, a client name only it sends, or its name.
func Claimed(claim string) (Harness, bool) {
	id := claimSlug(claim)
	if id == "" {
		return Harness{}, false
	}
	for _, h := range all {
		names := append([]string{h.ID, h.Claim, slug(h.Name)}, h.Aliases...)
		if h.Unsure == "" {
			for _, c := range h.Clients {
				names = append(names, slug(c))
			}
		}
		for _, n := range names {
			if n != "" && n == id {
				return h, true
			}
		}
	}
	return Harness{}, false
}

// ClaimName is a claim as Wakeshift records it: the ID of the harness it
// names (Claimed), else the claim as an ID ("My Agent" → my-agent), or ""
// if there's nothing of it.
func ClaimName(claim string) string {
	if h, ok := Claimed(claim); ok {
		return h.ID
	}
	return claimSlug(claim)
}

// claimSlug is a claim as an ID ("" for none, or one that's too long).
func claimSlug(claim string) string {
	claim = strings.TrimSpace(claim)
	if claim == "" || len(claim) > 200 {
		return ""
	}
	id := slug(claim)
	if id == "agent" && !strings.EqualFold(claim, "agent") {
		return "" // fewer than 2 letters or digits
	}
	return id
}

// Outcome is what a claim did.
type Outcome string

const (
	// NoClaim: the call didn't say.
	NoClaim Outcome = ""
	// Confirmed: it's what the call was recognized as.
	Confirmed Outcome = "confirmed"
	// Refined: one of the recognized harness's Family (Cursor → Grok Bot).
	Refined Outcome = "refined"
	// Decided: nothing recognized the call (or only a client name others
	// send too, Unsure), so the claim names its harness.
	Decided Outcome = "decided"
	// Unlisted: nothing recognized the call, and the claim names a program
	// Wakeshift doesn't know.
	Unlisted Outcome = "unlisted"
	// Ignored: it contradicts what the call was recognized as.
	Ignored Outcome = "ignored"
)

// Refine is a call's harness given what it was recognized as (detected,
// ok: Recognize) and what its agent claims. Recognition comes first (the
// client name, then _meta, then the User-Agent); the claim only refines
// it within its Family, or decides when nothing (or only an Unsure client
// name) recognized the call.
func Refine(detected Harness, ok bool, claim string) (Harness, Outcome) {
	if ClaimName(claim) == "" {
		return detected, NoClaim
	}
	c, known := Claimed(claim)
	switch {
	case !ok || detected.Unsure != "":
		if !known {
			return Harness{}, Unlisted // not Unsure's harness either
		}
		return c, Decided
	case !known:
		return detected, Ignored
	case c.ID == detected.ID:
		return detected, Confirmed
	case c.Family != "" && c.Family == detected.ID:
		return c, Refined
	}
	return detected, Ignored
}

// AskNote asks the agent to say which program it runs in and its
// conversation's ID, when they'd change its instructions.
func AskNote() string {
	return "To give you the right instructions, call this again with " + HarnessArg + " and " + ConversationArg + "."
}
