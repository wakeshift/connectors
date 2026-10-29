package harnesses

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The TypeScript and Python SDKs' wrapped tools ask Wakeshift for a call's
// block only when something names the conversation: a session_key, a
// conversation_id, or a harness's conversation in the call's _meta (as
// appkit does with Conversation). conversationMetaGolden lists those _meta
// keys (ConversationMeta, dotted for nested objects); this test checks it's
// current (go test -run TestConversationMetaGolden -update-meta rewrites
// it), and the SDKs' tests read it.
const conversationMetaGolden = "../../sdk/typescript/test/conversation-meta.json"

var updateConversationMeta = flag.Bool("update-meta", false, "rewrite the SDKs' conversation _meta golden file")

func TestConversationMetaGolden(t *testing.T) {
	if _, err := os.Stat(filepath.Dir(conversationMetaGolden)); err != nil {
		t.Skip("the TypeScript SDK isn't in this tree")
	}
	var keys []string
	for _, h := range All() {
		if h.ConversationMeta != "" && !slices.Contains(keys, h.ConversationMeta) {
			keys = append(keys, h.ConversationMeta)
		}
	}
	slices.Sort(keys)
	b, err := json.MarshalIndent(map[string]any{"keys": keys}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if *updateConversationMeta {
		if err := os.WriteFile(conversationMetaGolden, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(conversationMetaGolden)
	if err != nil {
		t.Fatalf("%v (go test ./internal/harnesses -run TestConversationMetaGolden -update-meta writes it)", err)
	}
	if string(have) != string(b) {
		t.Fatalf("%s is out of date: a harness's ConversationMeta changed. Run go test ./internal/harnesses -run TestConversationMetaGolden -update-meta", conversationMetaGolden)
	}
}
