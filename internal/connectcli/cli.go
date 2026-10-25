// Package connectcli is the wakeshift command on users' machines (built from
// cmd/wakeshift-connector, installed as ~/.wakeshift/bin/wakeshift by
// /install/<harness>.sh):
//
//	wakeshift install --harness NAME --issuer URL [--yes]   trust a Wakeshift and add the harness's hooks
//	                                                (asks first for an issuer not yet trusted, unless --yes)
//	wakeshift uninstall --harness NAME              remove them
//	wakeshift hook --harness NAME [--rewake]        what the harness runs (its own hook input on stdin)
//	wakeshift bridge --harness opencode             the opencode plugin's child (JSON lines)
//	wakeshift harnesses                             the harnesses it knows
//	wakeshift run --thread ID | status ID           Codex's per-thread connector
//	wakeshift version
//
// A hook without --harness is Codex's: what installs before this one put in
// ~/.codex/hooks.json (`wakeshift-codex hook`), until they're reinstalled.
package connectcli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wakeshift/connectors/internal/codexconnector"
	"github.com/wakeshift/connectors/internal/connector"
	"github.com/wakeshift/connectors/internal/harnesses"
	"github.com/wakeshift/connectors/internal/version"
)

// Confirm asks the person at the terminal a yes/no question (on /dev/tty:
// stdin is the install script itself when it's piped to sh). Tests replace
// it.
var Confirm = func(question string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errors.New("no terminal")
	}
	defer tty.Close()
	fmt.Fprint(tty, question)
	answer, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && answer == "" {
		return false, err
	}
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "y" || a == "yes", nil
}

// lease: a Codex connector whose thread nobody but it has used for this
// long stops (the app's next result for the thread starts it again).
const lease = time.Hour

// Main runs the command and exits.
func Main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, os.Args, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// Run runs the command with args (args[0] is the program).
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	prog := filepath.Base(args[0])
	usage := func() int {
		fmt.Fprintf(stderr, "usage: %s install --harness NAME --issuer URL [--yes] | uninstall --harness NAME | hook --harness NAME [--rewake] | bridge --harness opencode | harnesses | version\n", prog)
		fmt.Fprintf(stderr, "harnesses: %s\n", strings.Join(connector.Names(), ", "))
		return 2
	}
	fail := func(err error) int {
		if err == nil {
			return 0
		}
		fmt.Fprintf(stderr, "%s: %v\n", prog, err)
		return 1
	}
	if len(args) < 2 {
		return usage()
	}
	bin := self(prog)
	cmd, rest := args[1], args[2:]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "the harness: "+strings.Join(connector.Names(), ", "))
	issuer := fs.String("issuer", "", "the Wakeshift whose beacons to trust, e.g. https://app.wakeshift.ai")
	rewake := fs.Bool("rewake", false, "hook: this is an async waiter that wakes the session (Claude Code's asyncRewake)")
	thread := fs.String("thread", "", "run: the Codex thread")
	event := fs.String("event", "", "hook: the event, for harnesses whose hook input doesn't say (antigravity)")
	wait := fs.Duration("wait", 0, "hook: how long a stop hook listens (default: the harness's)")
	yes := fs.Bool("yes", false, "install: trust --issuer without asking (the install script from that Wakeshift passes it)")
	switch cmd {
	case "install", "uninstall", "hook", "bridge", "run":
		if fs.Parse(rest) != nil {
			return 2
		}
	}
	*harness = strings.ToLower(*harness)
	if cmd == "hook" && *harness == "" {
		*harness = "codex"
	}
	if (cmd == "install" || cmd == "uninstall") && *harness == "" {
		return fail(errors.New("--harness is required (one of: " + strings.Join(connector.Names(), ", ") + ")"))
	}

	switch cmd {
	case "install":
		if *issuer == "" {
			return fail(errors.New("--issuer is required"))
		}
		spec, ok := connector.Lookup(*harness)
		if !ok {
			return fail(unknown(*harness))
		}
		iss, err := connector.CheckIssuer(*issuer)
		if err != nil {
			return fail(err)
		}
		// A new issuer can wake this harness's conversations and have them
		// act on its messages: ask, so a command someone else wrote (or an
		// agent was told to run) doesn't add one silently.
		if !*yes && !slices.Contains(connector.LoadSettings(*harness).Issuers, iss) {
			ok, err := Confirm(fmt.Sprintf("Trust %s to wake your %s conversations on this machine? Only say yes if you're installing Wakeshift from that address. [y/N] ", iss, *harness))
			if err != nil {
				return fail(fmt.Errorf("trusting %s needs your confirmation: run this in a terminal (or pass --yes): %w", iss, err))
			}
			if !ok {
				return fail(errors.New("not installed: " + iss + " wasn't trusted"))
			}
		}
		if err := spec.Install(bin, iss, stdout); err != nil {
			return fail(err)
		}
		if h, ok := harnesses.Get(*harness); ok && h.Trust != "" {
			fmt.Fprintln(stdout, "Next: "+h.Trust)
		}
		return 0
	case "uninstall":
		spec, ok := connector.Lookup(*harness)
		if !ok {
			return fail(unknown(*harness))
		}
		return fail(spec.Uninstall(bin, stdout))
	case "hook":
		if *harness == "codex" { // also the hook earlier installs run: `wakeshift-codex hook`
			h := &codexconnector.Hook{Settings: codexconnector.LoadSettings(), Socket: codexconnector.Socket(),
				Start: func(thread, link string) error { return codexconnector.StartDetached(bin, thread, link) }}
			h.Run(ctx, stdin, stdout)
			return 0
		}
		spec, ok := connector.Lookup(*harness)
		if !ok || spec.Format == nil {
			return 0 // a hook never fails the harness's call
		}
		h := spec.NewHook(nil, nil)
		h.Rewake, h.EventName, h.Alive = *rewake, *event, connector.HarnessWatch()
		if _, deferred := spec.Format.(connector.DeferredFormat); deferred {
			// A result written after its hook returns (Antigravity): a
			// resolver redeems its beacon as soon as it's there.
			h.ResolveSoon = func(conversation string) { connector.StartResolver(bin, *harness, conversation) }
		}
		if *wait > 0 {
			h.StopWait = *wait
		}
		if *harness == "claude-code" && *rewake {
			if connector.ClaudeUnattended() {
				return 0 // -p: nothing to wake
			}
			h.WaiterKey = os.Getenv("CLAUDE_PID")
		}
		return h.Run(ctx, stdin, stdout, stderr)
	case "bridge":
		if *harness != "opencode" {
			return fail(errors.New("bridge is for --harness opencode"))
		}
		spec, _ := connector.Lookup("opencode")
		b := &connector.Bridge{Hook: spec.NewHook(nil, nil)}
		return fail(b.Run(ctx, stdin, stdout))
	case "harnesses":
		for _, h := range harnesses.All() {
			how := string(h.Wake)
			switch {
			case h.ReplacedBy != "":
				how = "retired: " + h.Blocker
			case !h.Installable:
				how = "not yet: " + h.Blocker
			}
			fmt.Fprintf(stdout, "%-12s %s (%s)\n", h.ID, h.Name, how)
		}
		return 0
	case "run":
		link, _ := bufio.NewReader(stdin).ReadString('\n')
		link = strings.TrimSpace(link)
		if *thread == "" || link == "" {
			return fail(errors.New("run needs --thread, and the stream link on stdin"))
		}
		log := slog.New(slog.NewTextHandler(stderr, nil))
		log.Info("connector: starting", "thread", *thread, "version", version.Version)
		err := codexconnector.Run(ctx, codexconnector.Config{Stream: link, Thread: *thread, Socket: codexconnector.Socket(),
			Lease: lease, Window: &codexconnector.Window{Thread: *thread}, Log: log})
		log.Info("connector: stopped", "why", err)
		return 0
	case "status":
		if len(rest) < 1 {
			return usage()
		}
		w := &codexconnector.Window{Thread: rest[0]}
		b, ok := w.Bind(ctx)
		if !ok {
			fmt.Fprintln(stdout, "Codex's log shows no Codex window for that thread (the connector relies on its lease).")
			return 0
		}
		open, why, err := w.Open(ctx, b)
		switch {
		case err != nil:
			fmt.Fprintln(stdout, "Couldn't read Codex's log:", err)
		case open:
			fmt.Fprintln(stdout, "A Codex window shows that thread.")
		default:
			fmt.Fprintln(stdout, "No Codex window shows that thread:", why+".")
		}
		return 0
	case "version":
		fmt.Fprintln(stdout, version.Version)
		return 0
	}
	return usage()
}

func unknown(h string) error {
	return fmt.Errorf("unknown harness %q (one of: %s)", h, strings.Join(connector.Names(), ", "))
}

// self is this binary's path, as installed (~/.wakeshift/bin/wakeshift).
// Not resolved through links: hook commands name the path the user
// installed, which survives updates.
func self(prog string) string {
	p, err := os.Executable()
	if err != nil {
		return filepath.Join(connector.Home(), "bin", prog)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}
