package harnesses

import (
	"net/url"
	"strings"
)

// Codex's install offer as a client request: a form the app's SDK shows in
// Codex's own dialog, so the user runs the install themselves and no model
// step is needed (Codex's default model doesn't relay the text offer). The SDK writes the message's first line itself
// ("Wakeshift, for <app>:") and enforces fixed bounds on what's here: a
// message of at most 1,200 characters and 20 lines, titles of at most 80,
// one enum field of at most 4 options.

// ElicitField is the form's one field; ElicitSetup and ElicitLater are its
// values. "Not now" comes first: Codex puts its cursor on the first option,
// so an Enter meant for the composer doesn't choose the install.
const (
	ElicitField = "choice"
	ElicitSetup = "setup"
	ElicitLater = "later"
)

// ElicitMessageMax and ElicitLinesMax are the SDKs' bounds on a form's
// message (the core's part; the SDK adds one line of its own).
const (
	ElicitMessageMax = 1200
	ElicitLinesMax   = 20
)

// FormSchema is an elicitation form's requested schema, in the only shape
// the SDKs run: an object of enum fields (string, oneOf titled constants).
type FormSchema struct {
	Type       string               `json:"type"` // "object"
	Required   []string             `json:"required"`
	Properties map[string]EnumField `json:"properties"`
}

// EnumField is one field of a FormSchema.
type EnumField struct {
	Type  string       `json:"type"` // "string"
	Title string       `json:"title"`
	OneOf []EnumOption `json:"oneOf"`
}

// EnumOption is one of an EnumField's values, with the title shown for it.
type EnumOption struct {
	Const string `json:"const"`
	Title string `json:"title"`
}

// CodexElicitSchema is the form's requested schema.
func CodexElicitSchema() FormSchema {
	return FormSchema{
		Type:     "object",
		Required: []string{ElicitField},
		Properties: map[string]EnumField{
			ElicitField: {Type: "string", Title: "Notifications in this conversation", OneOf: []EnumOption{
				{Const: ElicitLater, Title: "Not now"},
				{Const: ElicitSetup, Title: "I'll set it up"},
			}},
		},
	}
}

// CodexElicitMessage is the form's message for app's Codex thread (its ID,
// if the call names it), with the install commands the Wakeshift at base
// offers, one per line, and the docs' Codex install section at site's
// /codex (wakeshift.ai/codex in production).
func CodexElicitMessage(base, site, app, thread string) string {
	codex, _ := Get("codex")
	return app + " sends its notifications through Wakeshift. To be told here when they arrive, even while you're away, install Wakeshift's plugin for Codex once. " +
		"Wakeshift makes this plugin, not " + app + ". It adds three hooks and changes Codex's config and plugin folders (~/.codex or your CODEX_HOME); " +
		"the hooks download Wakeshift's connector to ~/.wakeshift/bin, check its SHA-256 and run it. " +
		"The exact commands are also at " + shortSite(site) + "/codex. Wakeshift never asks for a password, key or code.\n" +
		"1. In a terminal (with the same CODEX_HOME as this Codex, if you set one), run:\n" +
		commandLines(codex, base) +
		"2. Restart Codex in this conversation: in the CLI, /quit, then " + resumeCommand(thread) + "; in the app or IDE, restart it and reopen this conversation.\n" +
		"3. When Codex asks, review the hooks and trust Wakeshift's three (or \"Trust all and continue\" if only those are listed).\n" +
		"Already installed it? Skip step 1: just restart Codex and trust Wakeshift's hooks."
}

// CodexElicitSetupText is what the call's result says once the user chose
// "I'll set it up": the steps first (the SDK places it before the app's own
// content, so Codex's transcript preview shows them once the dialog has
// closed), then what the agent should do.
func CodexElicitSetupText(base, thread string) string {
	codex, _ := Get("codex")
	return "To set up notifications here, run in a terminal:\n" + commandLines(codex, base) +
		"then restart Codex (" + resumeCommand(thread) + ") and trust Wakeshift's hooks.\n" +
		"In Codex's dialog, the user chose to set up Wakeshift's plugin for Codex themselves, with the steps above. Don't run anything for it unless they ask you to; finish the task as usual. " +
		"After they restart Codex, trust the hooks and send a message, a note starting \"" + Connected + "\" appears."
}

// CodexElicitLaterText is what the call's result says once the user chose
// "Not now".
func CodexElicitLaterText() string {
	return "In Codex's dialog, the user chose not to set up notifications in this conversation: nothing will wake it. Keep doing what the user asked, and don't bring it up again."
}

// CodexElicitPendingNote is what another subscribing result says while the
// dialog is open.
func CodexElicitPendingNote() string {
	return "Subscribed. A question about notifications is open in Codex's dialog; don't ask the user about it yourself."
}

// CodexElicitAskedNote is what a later subscribing result says, while
// nothing wakes the conversation, after the user answered the dialog with
// choice; ok is false for an answer that leaves today's AskedNote.
func CodexElicitAskedNote(choice, base, thread string) (note string, ok bool) {
	codex, _ := Get("codex")
	switch choice {
	case ElicitSetup:
		return "Subscribed. In Codex's dialog, the user chose to set up Wakeshift's plugin for Codex themselves. Until it connects, nothing wakes this conversation; don't run anything for it unless they ask you to. " +
			"If they ask for the steps, they are: run " + strings.Join(commands(codex, base), " and then ") + " in a terminal, restart Codex (" + resumeCommand(thread) + "), and trust Wakeshift's hooks. " +
			"If it's installed but hasn't connected, they run /hooks, trust Wakeshift's three hooks, and send a message.", true
	case ElicitLater:
		return "Subscribed. The user chose not to set up notifications in this conversation, so nothing wakes it: keep doing what they asked, and don't bring it up.", true
	}
	return "", false
}

// commands is the install a harness's offer gives (OfferCommands), as the
// user types it: one command at a time.
func commands(h Harness, base string) []string { return OfferCommands(Route(h, base), base) }

func commandLines(h Harness, base string) string {
	var b strings.Builder
	for _, c := range commands(h, base) {
		b.WriteString("   " + c + "\n")
	}
	return b.String()
}

func resumeCommand(thread string) string {
	if thread == "" || len(thread) > 100 || strings.ContainsAny(thread, " \t\n\"'`$;&|") {
		return "codex resume --last"
	}
	return "codex resume " + thread
}

// shortSite is the site's host for the dialog ("wakeshift.ai"), the
// production site's when the core doesn't know its own.
func shortSite(site string) string {
	if u, err := url.Parse(site); err == nil && u.Host != "" {
		return u.Host
	}
	return "wakeshift.ai"
}
