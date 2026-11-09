package harnesses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"sort"
	"testing"
)

// What every harness's agent is told, pinned: textsGolden holds a SHA-256 prefix
// of each text the notes give, per harness and Wakeshift, so a change meant for one
// harness can't change another's by accident. A change to a text that's
// meant fails here; rewrite the file with go test ./internal/harnesses -run
// TestTextsGolden -update-texts, and check its diff touches only what was
// meant. A harness added to the table is added to the file then; until
// it is, it isn't checked here (its own tests pin its texts).
const textsGolden = "testdata/texts.json"

var updateTexts = flag.Bool("update-texts", false, "rewrite the harnesses' texts golden file")

// goldenTexts is every text, keyed "<harness>|<what>|<base>" (or
// "*|<what>" for the ones that aren't per harness).
func goldenTexts() map[string]string {
	const url = "https://core.test/v1/stream/wait?link=wsl_abc"
	out := map[string]string{
		"*|ArgDescription":        ArgDescription(),
		"*|CantWake":              CantWake(""),
		"*|MonitorRunningNote":    MonitorRunningNote(),
		"*|MonitorLoop":           MonitorLoop(url),
		"*|MonitorRules":          MonitorRules,
		"*|MonitorRulesBrief":     MonitorRulesBrief,
		"*|NoMonitorNote":         NoMonitorNote(url, false),
		"*|NoMonitorNote.brief":   NoMonitorNote(url, true),
		"*|CursorCloudNote":       CursorCloudNote(),
		"*|UntilLoop":             UntilLoop(url, true),
		"*|UntilLoop.nohooks":     UntilLoop(url, false),
		"*|SessionDescription":    SessionDescription,
		"*|ConversationDescriptn": ConversationDescription,
	}
	for _, h := range all {
		for _, base := range []string{ProductionIssuer, stagingIssuer, "http://localhost:8370"} {
			k := func(what string) string { return h.ID + "|" + what + "|" + base }
			out[k("InstallNote")] = InstallNote(h, base)
			out[k("SubscribeNote")] = SubscribeNote(h, base)
			out[k("RepeatNote")] = RepeatNote(h, base)
			out[k("AskedNote")] = AskedNote(h, base)
			out[k("PluginFirstNote")] = PluginFirstNote(h, base, "CI", false, "FALLBACK")
			out[k("PluginFirstNote.tasks")] = PluginFirstNote(h, base, "CI", true, "FALLBACK")
			out[k("PluginPendingNote")] = PluginPendingNote(h, base, false, "AGAIN")
			out[k("PluginAskedNote")] = PluginAskedNote(h, base, "CI")
			out[k("PluginConnectingNote")] = PluginConnectingNote(h, base, false)
			out[k("BackgroundUpgradeNote")] = BackgroundUpgradeNote(h, base)
			out[k("ConnectedNote")] = ConnectedNote(h, base)
			out[k("DeliversNote")] = DeliversNote(h, base, "CI")
			out[k("UpdateNote")] = UpdateNote(h, base)
			out[k("UpdateRequired")] = UpdateRequired(h, base)
			out[k("KitFor")] = KitFor(h, base)
		}
		k := func(what string) string { return h.ID + "|" + what }
		out[k("BackgroundNote")] = BackgroundNote(h, "CI", url, false)
		out[k("BackgroundNote.ask")] = BackgroundNote(h, "CI", url, true)
		out[k("BackgroundAgainNote")] = BackgroundAgainNote(h, "CI", url, false, false)
		out[k("BackgroundAgainNote.hooks")] = BackgroundAgainNote(h, "CI", url, true, true)
		out[k("BackgroundRunningNote")] = BackgroundRunningNote(h, "CI", url)
		out[k("ForegroundNote")] = ForegroundNote(h, "CI", url, true)
		out[k("UninstallNote")] = UninstallNote(h)
		out[k("TrustNote")] = TrustNote(h)
		out[k("RetiredNote")] = RetiredNote(h)
		out[k("CantWake")] = CantWake(h.Name)
		out[k("PluginReadyNote")] = PluginReadyNote(h, "CI", false)
		out[k("PluginReadyNote.hooks")] = PluginReadyNote(h, "CI", true)
		out[k("WaitNote")] = WaitNote(h, "CI", "~/.wakeshift/bin/wakeshift wait", true)
		out[k("WaitAgainNote")] = WaitAgainNote(h, "CI", "~/.wakeshift/bin/wakeshift wait", false)
		out[k("WaitRunningNote")] = WaitRunningNote(h, "CI", "~/.wakeshift/bin/wakeshift wait")
	}
	return out
}

func TestTextsGolden(t *testing.T) {
	texts := goldenTexts()
	got := map[string]string{}
	for k, v := range texts {
		sum := sha256.Sum256([]byte(v))
		got[k] = hex.EncodeToString(sum[:8])
	}
	if *updateTexts {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(textsGolden, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(textsGolden)
	if err != nil {
		t.Fatalf("%v (go test ./internal/harnesses -run TestTextsGolden -update-texts writes it)", err)
	}
	var want map[string]string
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: gone", k)
		} else if g != want[k] {
			t.Errorf("%s changed; it's now: %s", k, texts[k])
		}
	}
}
