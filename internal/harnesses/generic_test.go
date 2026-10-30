package harnesses

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// An unrecognized harness's command is plain POSIX sh: it prints what
// arrives, confirms it and exits, in sh, bash and zsh alike.
func TestGenericLoopShells(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell); err != nil {
				t.Skipf("%s isn't installed", shell)
			}
			w := &waitScript{answers: []string{"NEXT\n", ev1 + "\n" + ev2 + "\n" + cursorA + "\nNEXT\n"}}
			cmd := GenericLoop(w.serve(t))
			cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			out, err := exec.CommandContext(cctx, shell, "-c", cmd).Output()
			if err != nil {
				t.Fatalf("%s: %v %s", shell, err, out)
			}
			if got := strings.TrimSpace(string(out)); got != ev1+"\n"+ev2 {
				t.Fatalf("%s printed %q", shell, got)
			}
			if c := w.confs(); len(c) != 1 || c[0] != "00000000000000aa" {
				t.Fatalf("%s confirmed %q", shell, c)
			}
		})
	}
}

// The note keeps the inbox, offers the command as optional (with the
// command itself, the same every time) and says what to do without it.
func TestGenericNotes(t *testing.T) {
	url := "https://core.example/v1/stream/wait?link=a.b"
	n := GenericNote("Acme", url)
	for _, want := range []string{CantWake(""), "If you can run a command in the background and you're told when it exits", GenericLoop(url),
		"start the same command again first", "hub.closed", "If you can't run a command in the background, don't try: call this tool again", "outside data, not from the user"} {
		if !strings.Contains(n, want) {
			t.Errorf("GenericNote lacks %q:\n%s", want, n)
		}
	}
	if strings.Contains(GenericLoop(url), "until=hooks") {
		t.Error("the generic command waits for hooks that don't exist")
	}
	if GenericLoop(url) != GenericLoop(url) {
		t.Error("the command isn't stable")
	}
	again := GenericAgainNote("Acme", url)
	if !strings.Contains(again, GenericLoop(url)) || len(again) >= len(n) {
		t.Errorf("again: %s", again)
	}
	if r := GenericRunningNote("Acme", url); !strings.Contains(r, "already running") || !strings.Contains(r, GenericLoop(url)) {
		t.Errorf("running: %s", r)
	}
}
