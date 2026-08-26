package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/maborak/mabo-ctl/internal/supervisor"
)

// notifyInterval is how often a resident front end re-reads the supervisor for
// crash transitions. Status is cheap — one round of concurrent probes for live
// services, exit records for dead ones — and three seconds bounds how long a
// death can go unannounced without turning the front end into a busy poller.
const notifyInterval = 3 * time.Second

// notifyBodyLimit caps the quoted log line in a notification. Desktop
// notifications are not a log reader; they are a doorbell.
const notifyBodyLimit = 120

// notifySend is the platform notification call, injected so tests can observe
// what would have been shown without popping a dialog on the developer's desk.
type notifySend func(ctx context.Context, title, body string) error

// notifier watches a supervisor from a resident front end and fires a desktop
// notification when a service DIES: any live phase transitioning into exited or
// failed. It exists because residency is the whole value of `--attach` and
// `serve` beyond what one-shot commands already do — the operator is sitting
// in front of this machine, and "api died at 3pm" is exactly the news they are
// there to receive.
//
// It reads [Supervisor.Status] rather than hooking the reaper, deliberately:
// status consults the on-disk exit record, so a death that happened while no
// mabo-ctl was resident is still seen by the next watcher, and the watcher
// stays out of the supervisor's internals entirely.
// statusSource is what a notifier watches: the one method of
// [supervisor.Supervisor] it needs. It exists so tests can substitute a canned
// supervisor; production callers always pass the real one.
type statusSource interface {
	Status(ctx context.Context) []supervisor.Status
}

type notifier struct {
	sup  statusSource
	send notifySend
	// warn receives a failed notification attempt. A console that cannot pop
	// a dialog must keep running, so this is a report, never an error path.
	warn io.Writer

	// last holds each service's phase at the previous poll, so only
	// TRANSITIONS fire. A watcher starting against an already-dead service
	// announces nothing: the start command printed that failure, and repeating
	// it as a popup adds noise, not news.
	last map[string]supervisor.Phase
}

// newNotifier builds a watcher over sup. send is normally
// [sendDesktopNotification]; tests substitute a recorder.
func newNotifier(sup statusSource, send notifySend) *notifier {
	return &notifier{sup: sup, send: send, warn: os.Stderr, last: make(map[string]supervisor.Phase)}
}

// watch polls until ctx is cancelled. It never returns an error: a failed
// notification (no osascript, no notify-send, headless session) must not take
// down a console that is otherwise working. Failures go to warn.
func (n *notifier) watch(ctx context.Context) {
	ticker := time.NewTicker(notifyInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.poll(ctx)
		}
	}
}

// poll reads status once and sends a notification for every live→dead
// transition since the previous poll.
func (n *notifier) poll(ctx context.Context) {
	for _, st := range n.sup.Status(ctx) {
		prev, seen := n.last[st.Name]
		n.last[st.Name] = st.Phase
		if !seen || !livePhase(prev) || !deadPhase(st.Phase) {
			continue
		}
		title := fmt.Sprintf("mabo-ctl: %s %s", st.Name, st.Phase)
		body := truncateRunes(strings.SplitN(st.Detail, "\n", 2)[0], notifyBodyLimit)
		if body == "" {
			body = "the process is gone; run mabo-ctl logs " + st.Name
		}
		if err := n.send(ctx, title, body); err != nil && n.warn != nil {
			fmt.Fprintf(n.warn, "mabo-ctl: notify: %v\n", err)
		}
	}
}

// livePhase reports whether p describes a service with a process behind it.
func livePhase(p supervisor.Phase) bool {
	switch p {
	case supervisor.PhaseRunning, supervisor.PhaseReady,
		supervisor.PhaseSlow, supervisor.PhaseDegraded:
		return true
	default:
		return false
	}
}

// deadPhase reports whether p describes a service that died without mabo-ctl
// stopping it. stopped is excluded on purpose: a deliberate stop is not news.
func deadPhase(p supervisor.Phase) bool {
	return p == supervisor.PhaseExited || p == supervisor.PhaseFailed
}

// truncateRunes cuts s to at most limit runes, marking the cut, so a stack
// trace line cannot flood a notification centre.
func truncateRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// sendDesktopNotification shows title/body through the one desktop mechanism
// per platform: osascript on darwin, notify-send on Linux. The argv-not-shell
// discipline and the unsupported-platform refusal are [openURL]'s, inherited.
//
// The body is attacker-influenced in the weak sense that a service's own output
// ends up in it, so it is truncated before it reaches the script builder and
// the builder escapes it rather than interpolating it into shell.
func sendDesktopNotification(ctx context.Context, title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		script := appleScriptNotification(title, body)
		cmd := exec.CommandContext(ctx, "osascript", "-e", script)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("osascript: %w", err)
		}
		return nil
	case "linux":
		cmd := exec.CommandContext(ctx, "notify-send", title, body)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("notify-send: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("desktop notifications are not supported on %s", runtime.GOOS)
	}
}

// appleScriptNotification builds the `display notification` invocation. body
// and title are escaped because they are interpolated INTO AN APPLESCRIPT
// STRING LITERAL, where a double quote would end the string early — the same
// class of bug as shell interpolation, one syntax over.
func appleScriptNotification(title, body string) string {
	esc := func(s string) string {
		s = strings.ReplaceAll(s, "\\", "\\\\")
		return strings.ReplaceAll(s, `"`, `\"`)
	}
	return fmt.Sprintf(`display notification "%s" with title "%s"`, esc(body), esc(title))
}
