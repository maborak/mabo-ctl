// Package repl implements mabo-ctl's line-oriented interactive console: a prompt
// that stays resident, runs mabo-ctl commands, and reports a crash the moment it
// happens.
//
// # It is a dispatcher, not a command surface
//
// Every line the user types is split into an argv vector and handed to the SAME
// command tree the one-shot CLI executes. There is no per-verb code in this
// package and there must never be one: `status`, `start api`, `logs web -f`,
// `exec api pytest`, `shell api`, `health`, `preflight`, `open`, `reset
// --force` and `config` all work here because they work on the command line,
// and they cannot drift from it because there is only one implementation.
//
// A fourth independently maintained list of verbs is precisely the drift that
// let two copies of the shell predecessor diverge, which is the reason this
// project exists. If this package grows a command table, it has become the
// thing it was written to avoid.
//
// The two exceptions are [session.serve] and [session.unserve], and they are
// exceptions for a structural reason rather than a convenience one: they manage
// a listener whose lifetime is the SESSION. `mabo-ctl serve` on the command line
// blocks until it is interrupted, which inside a prompt would mean a console
// you can only stop by losing the prompt. So the REPL owns the socket, `serve`
// binds it once and prints the URL, and `unserve` releases it.
//
// # Re-entrancy
//
// Two hazards, both of which bite immediately if they are ignored.
//
// The command tree is built PER LINE, not once per session. Flag values in that
// tree are sticky — a tree reused across lines would leak `--all` from one
// `start` into the next one — so [Dispatcher.Dispatch] is contracted to build a
// fresh tree, and repl_test.go asserts a flag set on one line is back to its
// default on the next.
//
// A command's error must RETURN, never end the process. One typo, one unknown
// service, one failed start must leave the user at the prompt with an error
// printed above it. This package never calls os.Exit and the tests assert that
// a cobra error and an unknown verb both keep the loop running.
//
// # Residency is the point
//
// A REPL sitting at a prompt is the one place mabo-ctl can notice a supervised
// process dying and say so. The supervisor persists an exit record when it
// reaps a child; this package polls [Monitor] and writes `api exited (code 1)`
// into the scrollback the moment the record appears, erasing and redrawing the
// prompt line so the notice never lands in the middle of what the user is
// typing. A notice raised while a command is running is queued and flushed when
// the prompt comes back, so it cannot cut a status block in half.
//
// # The prompt carries no live counter
//
// The prompt is `mabo-ctl(<repo>)> ` and nothing else. A count of running
// services CANNOT be live behind a blocking line reader: it is drawn once and
// is wrong the instant a service dies, which is the failure mode this whole
// tool exists to remove. Recomputing it per line would cost a full
// supervisor.Status — health probes and an lsof per stopped-but-ported service
// — on every keystroke-completed line, to produce a number that is stale again
// before it is read. The counter is therefore OMITTED: `status` answers that
// question truthfully on demand, and the crash watcher volunteers the only
// change worth interrupting for.
//
// # Layering
//
// This package imports neither os/exec nor syscall, and nothing of mabo-ctl's own
// — not the supervisor, not cobra, not the HTTP server. It knows a service only
// as the name on a [Status]. [Dispatcher], [Listener] and [Monitor] are
// supplied by cmd/mabo-ctl, which is the layer allowed to build a command tree,
// bind a socket and drive processes. Signals are likewise not handled here:
// [Options.Interrupts] is a plain channel the caller feeds.
package repl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Command is one dispatchable command, as `help` lists it.
type Command struct {
	// Name is the word the user types.
	Name string
	// Short is the one-line description shown beside it.
	Short string
}

// Dispatcher runs one command line against mabo-ctl's command tree.
//
// It is an interface so this package can be tested without cobra, and so the
// cobra tree stays in cmd/mabo-ctl where the rest of the CLI wiring lives.
type Dispatcher interface {
	// Commands lists the dispatchable commands for `help`, in the order they
	// should be shown.
	Commands() []Command
	// Dispatch executes argv and returns whatever the command returned.
	//
	// Implementations MUST build a fresh command tree per call, or reset every
	// flag by hand: flag values persist on a cobra command, so a reused tree
	// leaks `--all` from one line into the next. Implementations MUST also
	// return errors rather than exiting the process — a typo may not end the
	// session.
	Dispatch(ctx context.Context, argv []string) error
}

// Listener is one bound web console whose lifetime the REPL owns.
//
// The methods are the ones *github.com/maborak/mabo-ctl/internal/web.Server
// already has, deliberately: this interface adds nothing, it only keeps
// internal/repl from importing the HTTP server.
type Listener interface {
	// Listen binds the socket and records the resolved address. It is called
	// once, before ListenAndServe, so the URL printed to the user is the
	// address a browser can actually open rather than the one that was asked
	// for.
	Listen() error
	// URL returns the address to open, session token included.
	URL() string
	// ListenAndServe serves until ctx is done, then shuts down gracefully.
	ListenAndServe(ctx context.Context) error
}

// Monitor reports the state of every supervised service. The REPL polls it to
// notice a process dying while the user sits at the prompt.
type Monitor interface {
	// Status reports the current phase of every service. It may probe, and
	// therefore may block.
	Status(ctx context.Context) []Status
}

// Status is the slice of a supervisor status the crash watcher reads.
//
// It is a local type rather than supervisor.Status so this package depends on a
// death record and not on the whole lifecycle API; cmd/mabo-ctl adapts one to the
// other in a dozen lines.
type Status struct {
	// Name is the declared service name.
	Name string
	// Dead reports that the service is gone without mabo-ctl having stopped it.
	Dead bool
	// Startup reports that the death happened before the service ever became
	// ready, which the CLI already announced in the foreground.
	Startup bool
	// ExitCode is the status it exited with, or negative when mabo-ctl never saw
	// the wait status.
	ExitCode int
	// ExitSignal names the signal that killed it, or "" when it exited on its
	// own.
	ExitSignal string
	// StartedAt is when mabo-ctl spawned the process that died. With ExitedAt it
	// identifies the RUN, so one death is announced exactly once.
	StartedAt time.Time
	// ExitedAt is when mabo-ctl observed the death, or the zero time when no
	// mabo-ctl was resident to see it.
	ExitedAt time.Time
}

// Options configures a session. Only Out and Commands are required; everything
// else degrades to a sensible inert default.
type Options struct {
	// Repo is the name shown in the prompt, normally the repository directory.
	// Empty renders the bare `mabo-ctl> ` prompt.
	Repo string
	// In is where lines are read from. Nil means an immediate end of input,
	// which quits.
	In io.Reader
	// Out receives the prompt, every notice, and every error this package
	// formats. Command output goes wherever the Dispatcher sends it, which in
	// production is the same stream.
	Out io.Writer
	// Commands is the command tree every non-native line is handed to.
	Commands Dispatcher
	// NewListener constructs the web console `serve` binds. addr is the
	// argument the user gave `serve`, or "" for the caller's default. Nil
	// disables both native verbs, which then say so instead of failing.
	NewListener func(addr string) (Listener, error)
	// Console is a web console the caller has ALREADY BOUND and already told
	// the user about, handed over for this session to serve.
	//
	// It exists for `mabo-ctl start --web-console`, which has to print the bound
	// URL before the prompt appears — the port is chosen by the kernel, so
	// there is nothing truthful to print until the socket exists — and must not
	// leave that socket behind when the prompt is left. Adopting it puts it
	// under the one lifetime this package already manages: `serve` reports it
	// instead of binding a second one, `unserve` stops it, and quitting stops
	// it, all through [session.shutdownServer]. The caller must therefore not
	// serve or close it itself; one listener with two owners is one listener
	// that leaks.
	//
	// Nil means the session binds nothing until somebody types `serve`.
	Console Listener
	// Watch is polled for crashes. Nil disables crash reporting.
	Watch Monitor
	// Poll is how often Watch is consulted. Zero means [DefaultPoll].
	Poll time.Duration
	// Interrupts receives one value per Ctrl-C. At the prompt it abandons the
	// line; during a command it cancels that command. Nil means no interrupt
	// handling — the caller owns signals, this package does not.
	Interrupts <-chan struct{}
	// Interactive reports that Out is a terminal, which is the condition for
	// erasing and redrawing the prompt line around an asynchronous notice.
	// False writes notices as plain lines, which is what a pipe and a test
	// want.
	Interactive bool
	// FormatError renders a command's error for the user. Nil means a plain
	// "error: " prefix; cmd/mabo-ctl supplies the colour-aware renderer so this
	// package does not grow a second copy of it.
	FormatError func(error) string
}

// DefaultPoll is how often the crash watcher asks the supervisor what is
// running when [Options.Poll] is zero.
//
// Two seconds is what the web console already polls at, and a status round is
// one concurrent batch of health probes rather than one per service. Slower
// would make "it died while I was reading the log" arrive late, which is the
// one thing residency is for.
const DefaultPoll = 2 * time.Second

// nativeVerbs are the words this package handles itself rather than dispatching.
//
// It is deliberately four entries long. serve and unserve are here because they
// own a socket whose lifetime is the session; help, quit and exit are here
// because they are the loop's own controls. Nothing else belongs on this list —
// see the package comment.
var nativeVerbs = []Command{
	{Name: "serve", Short: "start the web console for this session and print its URL"},
	{Name: "unserve", Short: "stop the web console and release its port"},
	{Name: "help", Short: "list these commands"},
	{Name: "quit", Short: "leave the console; the services keep running (Ctrl-D does the same)"},
}

// Run reads lines from opt.In and executes them until the user quits, input
// ends, or ctx is cancelled.
//
// It returns nil for a normal exit — a `quit`, an `exit`, or end of input — and
// an error only when the input stream itself failed. A command that fails does
// NOT end the session and does not produce an error here; that is the whole
// point of a prompt.
//
// Anything the session bound is released before Run returns: a console started
// with `serve` is shut down and its port freed. The supervised services are
// not: they were spawned detached and quitting a console is closing a window.
func Run(ctx context.Context, opt Options) error {
	s := newSession(ctx, opt)
	defer s.close()
	return s.loop()
}

// session is one run of the prompt.
type session struct {
	opt Options
	// ctx bounds the whole session: the crash watcher, the served console, and
	// every command line derive from it.
	ctx    context.Context
	stop   context.CancelFunc
	out    *printer
	in     *bufio.Reader
	prompt string

	mu sync.Mutex
	// cancel cancels the command currently executing, and is nil at the prompt.
	cancel context.CancelFunc
	// busy reports that a command is executing, so a crash notice is queued
	// rather than printed into the middle of a status block.
	busy bool
	// pending holds notices raised while busy, flushed before the next prompt.
	pending []string
	// srv is the console `serve` bound, or nil.
	srv *serving

	// watchDone closes when the crash watcher has returned.
	watchDone chan struct{}
}

// newSession builds a session from opt, filling in every optional field.
func newSession(ctx context.Context, opt Options) *session {
	if opt.Out == nil {
		opt.Out = io.Discard
	}
	if opt.In == nil {
		opt.In = strings.NewReader("")
	}
	if opt.Poll <= 0 {
		opt.Poll = DefaultPoll
	}
	if opt.FormatError == nil {
		opt.FormatError = func(err error) string { return "error: " + err.Error() }
	}

	sctx, stop := context.WithCancel(ctx)
	s := &session{
		opt:    opt,
		ctx:    sctx,
		stop:   stop,
		out:    &printer{w: opt.Out, redraw: opt.Interactive},
		in:     bufio.NewReader(opt.In),
		prompt: promptFor(opt.Repo),
	}
	return s
}

// promptFor renders the prompt for a repository name.
//
// The repository name is in it because a mabo-ctl session is per-repo and two
// terminals in two checkouts look identical otherwise. Nothing else is: see the
// package comment on why there is no service counter.
func promptFor(repo string) string {
	if repo == "" {
		return "mabo-ctl> "
	}
	return "mabo-ctl(" + repo + ")> "
}

// loop runs the prompt until the user leaves or input ends.
func (s *session) loop() error {
	s.adopt()
	s.out.line(s.banner())
	s.startWatch()
	go s.watchInterrupts()

	for {
		s.flush()
		s.out.showPrompt(s.prompt)

		line, err := s.in.ReadString('\n')
		ended := errors.Is(err, io.EOF)
		if err != nil && !ended {
			s.out.promptEOF()
			return fmt.Errorf("repl: read input: %w", err)
		}
		if strings.HasSuffix(line, "\n") {
			// The terminal echoed the newline, so the cursor is already at the
			// start of a fresh line and nothing has to be closed off.
			s.out.promptEntered()
		} else {
			s.out.promptEOF()
		}

		if strings.TrimSpace(line) != "" {
			if s.execute(line) {
				break
			}
		}
		if ended {
			break
		}
	}

	s.out.line(farewell)
	return nil
}

// farewell is the exit line. It says the services are still running because the
// opposite assumption — "I quit the console, so my dev servers are down" —
// produces a second session where every port is mysteriously held. Children are
// spawned detached; leaving is closing a window.
const farewell = `leaving the console. The services mabo-ctl started are STILL RUNNING — they were
spawned detached and outlive this session. Type "stop" before quitting, or run
"mabo-ctl stop" from any terminal, to take them down.`

// banner is what the session prints before its first prompt.
func (s *session) banner() string {
	return `mabo-ctl interactive console. Every line is run by the same mabo-ctl command tree
the CLI uses, so anything that works as "mabo-ctl <line>" works here.
Type "help" for the list, "quit" or Ctrl-D to leave. Quitting leaves the
services running.`
}

// execute runs one line and reports whether the session should end.
func (s *session) execute(line string) (quit bool) {
	argv, err := tokenize(line)
	if err != nil {
		s.out.line(s.opt.FormatError(err))
		return false
	}
	if len(argv) == 0 {
		return false
	}

	switch argv[0] {
	case "quit", "exit":
		return true
	case "help", "?":
		s.out.line(s.helpText())
		return false
	case "serve":
		s.serve(argv[1:])
		return false
	case "unserve":
		s.unserve(argv[1:])
		return false
	}

	if err := s.dispatch(argv); err != nil {
		s.out.line(s.opt.FormatError(err))
		if !s.known(argv[0]) {
			s.out.line(s.validSet())
		}
	}
	return false
}

// dispatch hands argv to the command tree under a context Ctrl-C can cancel.
//
// The context is per LINE. Cancelling it stops that command and nothing else,
// which is what makes Ctrl-C during a long start return to the prompt instead
// of ending the session.
func (s *session) dispatch(argv []string) error {
	if s.opt.Commands == nil {
		return errors.New("no command tree is wired into this console")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.setRunning(cancel)
	err := s.opt.Commands.Dispatch(ctx, argv)
	s.setIdle()
	cancel()
	return err
}

// setRunning marks the session busy and records how to cancel the running
// command.
func (s *session) setRunning(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy, s.cancel = true, cancel
}

// setIdle marks the session back at the prompt.
func (s *session) setIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy, s.cancel = false, nil
}

// watchInterrupts turns each Ctrl-C into a cancelled command, or — at the
// prompt — into an abandoned line.
//
// Abandoning the line is the terminal's doing rather than this package's: the
// tty discards the pending input when it raises SIGINT, so all that is left to
// do is close off the prompt and draw a fresh one. The blocking read stays
// blocked, which is correct — there is nothing to read yet.
func (s *session) watchInterrupts() {
	if s.opt.Interrupts == nil {
		return
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case _, ok := <-s.opt.Interrupts:
			if !ok {
				return
			}
			s.mu.Lock()
			cancel := s.cancel
			s.mu.Unlock()
			s.out.notice("^C")
			if cancel != nil {
				cancel()
			}
		}
	}
}

// announce prints an asynchronous line, or queues it when a command is running
// so that it cannot arrive in the middle of that command's output.
func (s *session) announce(line string) {
	s.mu.Lock()
	if s.busy {
		s.pending = append(s.pending, line)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.out.notice(line)
}

// flush prints everything announce queued while a command was running.
func (s *session) flush() {
	s.mu.Lock()
	lines := s.pending
	s.pending = nil
	s.mu.Unlock()
	for _, l := range lines {
		s.out.notice(l)
	}
}

// known reports whether verb is a word this console recognises: one of its own,
// or one of the dispatched commands.
//
// It is NOT a validity check on the line — a bare service name is the `mabo-ctl
// <svc>` start shorthand and is valid without being a command. It decides only
// whether a failing line deserves the command list printed under its error.
func (s *session) known(verb string) bool {
	for _, c := range nativeVerbs {
		if c.Name == verb {
			return true
		}
	}
	if verb == "exit" || verb == "?" {
		return true
	}
	if s.opt.Commands == nil {
		return false
	}
	for _, c := range s.opt.Commands.Commands() {
		if c.Name == verb {
			return true
		}
	}
	return false
}

// validSet is the one-line list of everything this console accepts, printed
// under the error from a word it does not recognise.
func (s *session) validSet() string {
	names := make([]string, 0, 16)
	for _, c := range s.dispatched() {
		names = append(names, c.Name)
	}
	for _, c := range nativeVerbs {
		names = append(names, c.Name)
	}
	names = append(names, "exit")
	return "commands: " + strings.Join(names, ", ") + `; or a service name to start it. Type "help" for details.`
}

// dispatched returns the command tree's commands with the native verbs removed.
//
// serve is in both lists — the tree has a `mabo-ctl serve` that blocks until it
// is interrupted, and this console has one that binds for the session — and the
// console's is the one that runs here, so listing the other would document a
// behaviour the user cannot get.
func (s *session) dispatched() []Command {
	if s.opt.Commands == nil {
		return nil
	}
	var out []Command
	for _, c := range s.opt.Commands.Commands() {
		native := false
		for _, n := range nativeVerbs {
			if n.Name == c.Name {
				native = true
				break
			}
		}
		if !native {
			out = append(out, c)
		}
	}
	return out
}

// helpText renders `help`.
func (s *session) helpText() string {
	var b strings.Builder
	b.WriteString("Commands, run by the same mabo-ctl command tree the CLI uses:\n")
	width := 0
	all := append(append([]Command(nil), s.dispatched()...), nativeVerbs...)
	for _, c := range all {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range s.dispatched() {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Name, c.Short)
	}
	b.WriteString("\nConsole commands, handled here because they belong to this session:\n")
	for _, c := range nativeVerbs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, c.Name, c.Short)
	}
	b.WriteString(`
Flags work exactly as they do on the command line: "logs web -f", "start --all",
"reset --force". A bare service name starts it. Ctrl-C abandons the line you are
typing, or cancels the command that is running.`)
	return b.String()
}

// close releases everything the session bound: the served console, the crash
// watcher, and the session context. It does not stop any supervised service.
func (s *session) close() {
	// The shutdown error is deliberately dropped: this runs as the session ends,
	// so there is no prompt left to report it to and nothing a reader could do
	// about it. `unserve` is the path that shuts the console down while the
	// session continues, and THAT one shows the error.
	_, _, _ = s.shutdownServer()
	s.stop()
	if s.watchDone != nil {
		<-s.watchDone
	}
}
