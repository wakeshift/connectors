package harnesses

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// noRepeat matches a note asking the agent to call the app's tool again
// (or to call one just to check, connect or confirm). The app's tool may
// have side effects: it sends a message or starts a paid job, so a repeat
// call repeats them. Where nothing wakes the conversation, the app's own
// tools are how the agent checks, as without Wakeshift; an install
// connects without a call. Muse's notes (Wake OneShot) and Capy's (Wake
// Webhook, Fallback OneShot) follow the same rule: a command run again,
// or a webhook watch, is how they keep listening, never a tool call.
// Calling Wakeshift's own wait tool again is how its listener works, so
// it isn't matched.
var noRepeat = regexp.MustCompile(`(?i)call (this|the app's|that|the) (app's )?tool(s)?( that [a-z ]+)? (again|once more)|call this tool|call (it|this) once more|call this again|to check for (them|messages)|once more, on its own|call the app's tool`)

// asksForCall is the first match of noRepeat in n that isn't a "don't" or
// "don't need to" ("" if none).
func asksForCall(n string) string {
	for _, loc := range noRepeat.FindAllStringIndex(n, -1) {
		before := strings.ToLower(n[max(0, loc[0]-16):loc[0]])
		if strings.HasSuffix(before, "don't ") || strings.HasSuffix(before, "need to ") {
			continue
		}
		return n[loc[0]:loc[1]]
	}
	return ""
}

// No note, for any harness (Capy and Muse included) and any state, asks
// for another call of the app's tools; nor does any of Capy's webhook-route
// notes (no watch, registered, dead, the fallback command first, again or
// running).
func TestNoNoteAsksForAnotherCall(t *testing.T) {
	const base, url, app, handle = "https://app.wakeshift.ai", "https://x.test/v1/stream/wait?link=a", "CI", "wsc_abc"
	notes := map[string]string{
		"CantWake":               CantWake(""),
		"CantWake goose":         CantWake("Goose"),
		"CursorCloudNote":        CursorCloudNote(),
		"KeylessUnsubscribeNote": KeylessUnsubscribeNote,
		"TaskSessionGone":        TaskSessionGone,
		"TaskNote":               TaskNote(app, handle),
		"TaskAgainNote":          TaskAgainNote(app, handle),
		"TaskRunningNote":        TaskRunningNote(handle),
		"KeepSessionNote":        KeepSessionNote(handle),
		"NoMonitorNote":          NoMonitorNote(url, false),
		"NoMonitorNote brief":    NoMonitorNote(url, true),
		"MonitorRunningNote":     MonitorRunningNote(),
	}
	const link = "https://x.test/v1/stream/webhook?link=t"
	for name, n := range map[string]string{
		"CapyWebhookNote":         CapyWebhookNote(app, link, url, false, false),
		"CapyWebhookNote again":   CapyWebhookNote(app, link, url, true, false),
		"CapyWebhookNote running": CapyWebhookNote(app, link, url, false, true),
		"CapyRegisteredNote":      CapyRegisteredNote(app),
		"CapyDeadNote":            CapyDeadNote(app, link, url, false),
		"CapyDeadNote running":    CapyDeadNote(app, link, url, true),
		"CapyRegistered":          CapyRegistered(app),
	} {
		notes[name] = n
	}
	// Grok Bot's webhook notes, every state.
	const ch, fg = "0b8f3c1e-2d4a-4b6c-8e0f-1a2b3c4d5e6f", "FOREGROUND."
	in := GrokInstall{Base: base, Command: "INSTALL-COMMAND"}
	for name, n := range map[string]string{
		"GrokKeyNote":            GrokKeyNote(app, in, "bot-1", ch, fg),
		"GrokWebhookNote":        GrokWebhookNote(app, base, "bot-1", link, 24*time.Hour, fg),
		"GrokJoinedNote":         GrokJoinedNote(app),
		"GrokKeyMissingNote":     GrokKeyMissingNote(app, in, "bot-1", ch, fg),
		"GrokRekeyNote":          GrokRekeyNote(link, fg),
		"GrokBadSignatureNote":   GrokBadSignatureNote(fg),
		"GrokChallengeGone":      GrokChallengeGone,
		"GrokPrivateKeyNote":     GrokPrivateKeyNote,
		"GrokBoundNote":          GrokBoundNote(app),
		"GrokSetupFailedNote":    GrokSetupFailedNote,
		"GrokSetupMismatchNote":  GrokSetupMismatchNote,
		"GrokRegisteredNote":     GrokRegisteredNote(app),
		"GrokPendingNote":        GrokPendingNote(app, link, time.Now(), in, "bot-1", ch, fg),
		"GrokDeadNote":           GrokDeadNote(app, "unauthorized", in, "bot-1", ch, link, 24*time.Hour, fg),
		"GrokStoppedLine":        GrokStoppedLine(app, "not_found"),
		"GrokRoutineInstruction": GrokRoutineInstruction(base, "bot-1"),
	} {
		notes[name] = n
	}
	// Every harness is covered, these two by name: they came after the
	// rule and once had a narrower workaround of their own.
	for _, id := range []string{"capy", "muse"} {
		if _, ok := Get(id); !ok {
			t.Fatalf("no %s", id)
		}
	}
	for _, h := range All() {
		for _, b := range []string{base, "http://localhost:8370"} {
			for name, n := range map[string]string{
				"InstallNote":           InstallNote(h, b),
				"SubscribeNote":         SubscribeNote(h, b),
				"RepeatNote":            RepeatNote(h, b),
				"AskedNote":             AskedNote(h, b),
				"PluginAskedNote":       PluginAskedNote(h, b, app),
				"PluginFirstNote":       PluginFirstNote(h, b, app, false, "FALLBACK."),
				"PluginFirstNote tasks": PluginFirstNote(h, b, app, true, "FALLBACK."),
				"PluginPendingNote":     PluginPendingNote(h, b, false, "AGAIN."),
				"PluginConnectingNote":  PluginConnectingNote(h, b, false),
				"PluginReadyNote":       PluginReadyNote(h, "App", false),
				"PluginReadyNote hooks": PluginReadyNote(h, "App", true),
				"UpdateNote":            UpdateNote(h, b),
				"UpdateRequired":        UpdateRequired(h, b),
				"RetiredNote":           RetiredNote(h),
				"ConnectedNote":         ConnectedNote(h, b),
				"DeliversNote":          DeliversNote(h, b, app),
				"UninstallNote":         UninstallNote(h),
				"TrustNote":             TrustNote(Route(h, b)),
				"BackgroundUpgradeNote": BackgroundUpgradeNote(h, b),
			} {
				notes[h.ID+" "+b+" "+name] = n
			}
		}
		for name, n := range map[string]string{
			"BackgroundNote":        BackgroundNote(h, app, url, true),
			"BackgroundNote known":  BackgroundNote(h, app, url, false),
			"BackgroundAgainNote":   BackgroundAgainNote(h, app, url, true, false),
			"BackgroundRunningNote": BackgroundRunningNote(h, app, url),
			"ForegroundNote":        ForegroundNote(h, app, url, true),
			"WaitNote":              WaitNote(h, app, "~/.wakeshift/bin/wakeshift wait", true),
			"WaitAgainNote":         WaitAgainNote(h, app, "~/.wakeshift/bin/wakeshift wait", true),
			"WaitRunningNote":       WaitRunningNote(h, app, "~/.wakeshift/bin/wakeshift wait"),
		} {
			notes[h.ID+" "+name] = n
		}
	}
	for name, n := range notes {
		if m := asksForCall(n); m != "" {
			t.Errorf("%s asks for another call (%q): %s", name, m, n)
		}
		if strings.Contains(n, "held for you") {
			t.Errorf("%s says messages are held for the agent: %s", name, n)
		}
	}
	// The pattern itself catches what the notes used to say.
	for _, old := range []string{"call this tool again to check for them", "Then call this tool again: a note confirms it",
		"call this tool once more, on its own (not alongside another call), to confirm", "Call the app's tool that subscribed you again",
		"To give you the right instructions, call this again with harness"} {
		if asksForCall(old) == "" {
			t.Errorf("not caught: %q", old)
		}
	}
}
