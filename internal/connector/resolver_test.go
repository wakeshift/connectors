package connector

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The resolver runs the hook command with --event resolve and the
// conversation on stdin.
func TestStartResolver(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	bin := filepath.Join(dir, "wakeshift")
	os.WriteFile(bin, []byte("#!/bin/sh\n{ echo \"$*\"; cat; } > "+out+".tmp && mv "+out+".tmp "+out+"\n"), 0o700)
	if err := StartResolver(bin, "antigravity", "conv-1"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(out)
		if err == nil {
			if got := string(b); !strings.Contains(got, "hook --harness antigravity --event resolve") || !strings.Contains(got, `{"conversationId":"conv-1"}`) {
				t.Fatalf("ran with %q", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the resolver didn't run")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
