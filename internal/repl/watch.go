package repl

import (
	"fmt"
	"time"
)

// death identifies one observed death of one service.
//
// It is a comparable value on purpose: the watcher announces a crash when this
// changes and stays quiet when it does not, which is what stops `api exited
// (code 1)` being reprinted every two seconds for the rest of the afternoon.
// StartedAt is in the key because ExitedAt is the ZERO TIME for the one case
// mabo-ctl cannot date — a process that died with no mabo-ctl resident to reap it —
// and without the spawn time two such deaths of the same service would be
// indistinguishable.
type death struct {
	code    int
	signal  string
	started time.Time
	ended   time.Time
}

// startWatch begins polling for deaths, if the caller supplied something to
// poll. It returns immediately; the watcher runs until the session context is
// cancelled and [session.close] waits for it.
func (s *session) startWatch() {
	if s.opt.Watch == nil {
		return
	}
	s.watchDone = make(chan struct{})
	go s.watchDeaths()
}

// watchDeaths polls the supervisor and announces every death it has not
// announced before.
//
// This is what residency buys, and it is the reason a REPL is worth having at
// all. A one-shot `mabo-ctl start` exits seconds after spawning, so a service
// that dies an hour later dies unwitnessed and the news waits for whenever
// somebody next types `mabo-ctl status`. A prompt sitting open is the one place
// mabo-ctl can notice the exit record appear and say so into the scrollback while
// the developer is still looking at the terminal.
//
// The FIRST poll is a baseline and announces nothing. A service that was
// already dead when the console opened is not news; it is the state the console
// was opened to look at, and `status` shows it in full.
//
// A death that happened during startup is likewise not announced. That is
// [Status.Startup] — the service never came up at all — and it is by
// construction something a `start` in this very session just printed in the
// foreground, complete with the log tail. Announcing it again two seconds later
// would be the console arguing with itself.
func (s *session) watchDeaths() {
	defer close(s.watchDone)

	tick := time.NewTicker(s.opt.Poll)
	defer tick.Stop()

	seen := map[string]death{}

	// Take the baseline NOW, not on the first tick. Blocking on the ticker first
	// meant every death in the opening poll interval was folded into the
	// baseline and suppressed — and because it landed in `seen`, it stayed
	// suppressed for the life of the session. A service that died in the two
	// seconds after the prompt opened was never announced at all. The blind
	// window is now the session's own startup rather than a whole poll.
	for _, st := range s.opt.Watch.Status(s.ctx) {
		if st.Dead {
			seen[st.Name] = death{
				code: st.ExitCode, signal: st.ExitSignal,
				started: st.StartedAt, ended: st.ExitedAt,
			}
		}
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}

		for _, st := range s.opt.Watch.Status(s.ctx) {
			if !st.Dead {
				delete(seen, st.Name)
				continue
			}
			d := death{
				code:    st.ExitCode,
				signal:  st.ExitSignal,
				started: st.StartedAt,
				ended:   st.ExitedAt,
			}
			if prev, ok := seen[st.Name]; ok && prev == d {
				continue
			}
			seen[st.Name] = d
			if !st.Startup {
				s.announce(crashLine(st))
			}
		}
	}
}

// crashLine is what a death looks like in the scrollback.
//
// It is one line and it names the service, because it is being printed into a
// stream of other output and possibly over the top of a prompt. How it died
// comes from the exit record; when there is no record the line says the status
// is unknown rather than inventing a clean exit, which is the one lie a crash
// report must never tell. The pointer to `logs` is there because the next thing
// the developer wants is the stack trace, and the log is where it is.
func crashLine(st Status) string {
	var how string
	switch {
	case st.ExitSignal != "":
		how = "killed by " + st.ExitSignal
	case st.ExitCode < 0:
		how = "exit status unknown"
	default:
		how = fmt.Sprintf("code %d", st.ExitCode)
	}
	return fmt.Sprintf("%s exited (%s) — mabo-ctl did not stop it; run \"logs %s\" to see why",
		st.Name, how, st.Name)
}
