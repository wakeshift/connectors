// Package connectcli is the wakeshift command on users' machines (built from
// cmd/wakeshift-connector, installed as ~/.wakeshift/bin/wakeshift by
// /install/<harness>.sh):
//
//	wakeshift install --harness NAME --issuer URL [--yes]   trust a Wakeshift and add the harness's hooks
//	                                                (asks first for an issuer not yet trusted, unless --yes)
//	wakeshift uninstall --harness NAME              remove them
//	wakeshift allow --harness NAME --app ID --tools A,B | --none
//	                                                pre-approve an app's tools the user agreed to (or not: remove them)
//	wakeshift hook --harness NAME [--rewake]        what the harness runs (its own hook input on stdin)
//	wakeshift wait                                  wait for this conversation's next messages (the agent runs it)
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

// AfterInstall is the installer's last line: an agent that ran it for its
// conversation calls the app's tool again, since the hooks connect a
// conversation only from a beacon in a result that comes after the install.
const AfterInstall = "In the conversation that asked for this install: tell the user the step above, if there is one, and once it's done (if it needs nothing from them, right away, without waiting for them) call the app's tool that offered the install again. " +
	"The hooks connect a conversation only from a tool result that comes after the install, so until that call they can't wake it."

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
		fmt.Fprintf(stderr, "usage: %s install --harness NAME --issuer URL [--yes] | uninstall --harness NAME | allow --harness NAME --app ID (--tools A,B | --none) | hook --harness NAME [--rewake] | wait | bridge --harness opencode | harnesses | version\n", prog)
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
	rearm := fs.Bool("rearm", false, "run: started again by the hook without a beacon: only if no connector runs for the thread")
	event := fs.String("event", "", "hook: the event, for harnesses whose hook input doesn't say (antigravity)")
	wait := fs.Duration("wait", 0, "hook: how long a stop hook listens (default: the harness's)")
	yes := fs.Bool("yes", false, "install: trust --issuer without asking (the install script from that Wakeshift passes it)")
	app := fs.String("app", "", "allow: the app whose tools to allow")
	tools := fs.String("tools", "", "allow: the app's tools the user agreed to allow without asking, comma-separated")
	none := fs.Bool("none", false, "allow: the user said no: remove the app's tool rules and don't offer them again")
	switch cmd {
	case "install", "uninstall", "hook", "bridge", "run", "allow":
		if fs.Parse(rest) != nil {
			return 2
		}
	}
	*harness = strings.ToLower(*harness)
	if cmd == "hook" && *harness == "" {
		*harness = "codex"
	}
	if (cmd == "install" || cmd == "uninstall" || cmd == "allow") && *harness == "" {
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
		// The user's yes to the install covered the app's tools the
		// question named (harnesses.WithTools): record it. Their rules
		// are written once the hooks connect a conversation to the app.
		if v := os.Getenv(harnesses.AllowToolsEnv); v != "" {
			if app, tools, err := connector.ParseAllowTools(v); err != nil {
				fmt.Fprintf(stdout, "Didn't pre-approve any app's tools: %s %v.\n", harnesses.AllowToolsEnv, err)
			} else if err := spec.AllowTools(app, tools, stdout); err != nil {
				fmt.Fprintf(stdout, "Didn't pre-approve %s's tools: %v. Wakeshift works without it; %s asks before each call.\n", app, err, spec.Name)
			}
		}
		if h, ok := harnesses.Get(*harness); ok {
			if t := harnesses.TrustNote(h); t != "" {
				fmt.Fprintln(stdout, "Next: "+t)
			}
			// An agent that ran this mid-conversation took "nothing to
			// approve" as done and waited (wakebench S11): the hooks
			// connect a conversation only from a beacon in a result that
			// comes after the install, so say it last, where it's read.
			fmt.Fprintln(stdout, AfterInstall)
		}
		return 0
	case "uninstall":
		spec, ok := connector.Lookup(*harness)
		if !ok {
			return fail(unknown(*harness))
		}
		return fail(spec.Uninstall(bin, stdout))
	case "allow":
		spec, ok := connector.Lookup(*harness)
		if !ok {
			return fail(unknown(*harness))
		}
		switch {
		case *app == "":
			return fail(errors.New("--app is required"))
		case *none == (*tools != ""):
			return fail(errors.New("give either --tools <tool>,<tool>… (the tools the user agreed to) or --none"))
		case *none:
			return fail(spec.DeclineTools(*app, stdout))
		}
		list, err := connector.ParseTools(*tools)
		if err != nil {
			return fail(err)
		}
		return fail(spec.AllowTools(*app, list, stdout))
	case "hook":
		if *harness == "codex" { // also the hook earlier installs run: `wakeshift-codex hook`
			set := codexconnector.LoadSettings()
			if !set.Installed() {
				return 0 // uninstalled: a session that read its hooks before keeps calling them
			}
			h := &codexconnector.Hook{Settings: set, Socket: codexconnector.Socket(),
				Start: func(thread, link, why string, rearm bool) error {
					return codexconnector.StartDetached(bin, thread, link, why, rearm)
				}}
			h.Run(ctx, stdin, stdout)
			return 0
		}
		spec, ok := connector.Lookup(*harness)
		if !ok || spec.Format == nil {
			return 0 // a hook never fails the harness's call
		}
		h := spec.NewHook(nil, nil)
		if !h.Verifier.Settings.Installed() {
			return 0 // uninstalled: a session that read its hooks before keeps calling them
		}
		h.Rewake, h.EventName, h.Alive, h.Bin = *rewake, *event, connector.HarnessWatch(), bin
		if spec.Wait {
			h.WaitCommand = connector.WaitCommand(bin)
		}
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
	case "wait":
		return waitCmd(ctx, rest, stdout, stderr, prog)
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
		links := connector.LinksFor(codexconnector.LoadSettings())
		err := codexconnector.Run(ctx, codexconnector.Config{Stream: link, Thread: *thread, Socket: codexconnector.Socket(),
			Lease: lease, Window: &codexconnector.Window{Thread: *thread}, Links: &links,
			Lock: codexconnector.RunLock(*thread), Exclusive: *rearm, StartGrace: 15 * time.Second, CodexGrace: time.Minute, Log: log})
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

// waitCmd is `wakeshift wait` (connector.Waiter): exactly that, no arguments,
// and never into a file the command line redirected it to, so a harness's
// rule for it (a prefix rule, in Antigravity) allows nothing more.
func waitCmd(ctx context.Context, rest []string, stdout, stderr io.Writer, prog string) int {
	if len(rest) != 0 {
		fmt.Fprintf(stderr, "%s: %v\n", prog, connector.ErrWaitArgs)
		return 2
	}
	for _, w := range []io.Writer{stderr, stdout} {
		if f, ok := w.(*os.File); ok {
			if err := connector.CheckWaitOutput(f); err != nil {
				if w == stderr {
					return 2 // nowhere safe to say so
				}
				fmt.Fprintf(stderr, "%s: %v\n", prog, err)
				return 2
			}
		}
	}
	t, ok := connector.FindWaitTarget(os.Getenv)
	if !ok {
		fmt.Fprintln(stdout, connector.ClosedLine("not_connected", connector.NotConnected))
		return 1
	}
	w := connector.NewWaiter(t, stdout)
	w.Alive = connector.HarnessWatch()
	return w.Run(ctx)
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
