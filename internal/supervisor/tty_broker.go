//go:build darwin || linux

package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/maborak/mabo-ctl/internal/service"
	"github.com/maborak/mabo-ctl/internal/state"
)

// ttyBrokerHandshakeTimeout bounds how long startOne waits for the broker to
// report the child's pid. A pty that cannot be allocated answers immediately;
// the timeout only covers a pathological hang, and a missed deadline fails the
// start rather than leaving the claim dangling.
const ttyBrokerHandshakeTimeout = 10 * time.Second

// ttyBrokerCommand is the hidden subcommand mabo-ctl runs as its terminal
// broker. Registered Hidden so help, completions and the schema catalogue
// never name it: plumbing between two of OUR processes, not a user verb.
const ttyBrokerCommand = "internal-tty-broker"

// TTYBrokerCommand exposes the hidden name to the command layer, so the
// registration and the spawn site cannot disagree.
var TTYBrokerCommand = ttyBrokerCommand

// ttyBrokerLogTailBytes bounds the output snapshot the broker keeps so an exit
// record written from OUTSIDE any resident reaper still carries evidence.
const ttyBrokerLogTailBytes = 8 * 1024

// ttyDetachByte is Ctrl-Q. The attach client consumes it instead of relaying;
// a service that genuinely needs a raw 0x11 through must live behind tty:false
// and be driven by `exec` instead. One unambiguous key was chosen over an
// escape-sequence scheme precisely so the trade-off stays documentable here.
const ttyDetachByte = 0x11

// ttyHandshake is what the broker writes to its stdout before anything else:
// either the real child's pid, or why there is none.
type ttyHandshake struct {
	PID int    `json:"pid"`
	Err string `json:"err,omitempty"`
}

// spawnTTY hands one service to a DETACHED broker process — this same binary,
// re-invoked as [ttyBrokerCommand] — which owns the pty for exactly as long as
// the service lives. Everything here exists so attach can be possible WITHOUT
// giving up the property the supervisor rests on: a supervised child survives
// the terminal that started it. The broker is setsid-detached from us; the
// CHILD is setsid again inside the broker, so stop-by-group semantics hold.
//
// On success it returns after reading the handshake — the real child's pid —
// leaving readiness polling identical to any other service. The exit status is
// recorded by the broker itself, being the only waiter, so exit records stay
// complete for tty services too.
func (s *Supervisor) spawnTTY(in service.Instance, logPath, sockPath string) (int, error) {
	self, err := ttyBrokerExecutable()
	if err != nil {
		return 0, fmt.Errorf("locate mabo-ctl: %w", err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("broker handshake pipe: %w", err)
	}
	defer pr.Close()
	defer pw.Close()

	c := exec.Command(self, ttyBrokerCommand, "--log", logPath, "--sock", sockPath, "--")
	c.Args = append(c.Args, in.Cmd...)
	c.Dir = in.Dir
	c.Env = in.Env
	c.Stdout = pw
	c.Stderr = pw
	setDetached(c)
	if serr := c.Start(); serr != nil {
		return 0, fmt.Errorf("spawn terminal broker: %w", serr)
	}
	pw.Close() // EOF at the broker's end matters, not ours

	type hsResult struct {
		hs  ttyHandshake
		err error
	}
	ch := make(chan hsResult, 1)
	go func() {
		var hs ttyHandshake
		b, rerr := bufioReadLine(pr)
		if rerr == nil {
			rerr = json.Unmarshal(b, &hs)
		}
		ch <- hsResult{hs, rerr}
	}()

	select {
	case <-time.After(ttyBrokerHandshakeTimeout):
		_ = c.Process.Kill()
		return 0, errors.New("terminal broker did not answer within " + ttyBrokerHandshakeTimeout.String())
	case res := <-ch:
		if res.err != nil {
			return 0, fmt.Errorf("terminal broker handshake: %w", res.err)
		}
		if res.hs.Err != "" {
			return 0, fmt.Errorf("terminal broker: %s", res.hs.Err)
		}
		if res.hs.PID <= 1 {
			return 0, fmt.Errorf("terminal broker reported implausible pid %d", res.hs.PID)
		}
		return res.hs.PID, nil
	}
}

// runTTYBrokerFromArgs is the process entry of the hidden broker subcommand.
// argv is everything after the subcommand name.
// ttyBrokerExecutable names the binary re-invoked as the broker. A variable
// ONLY because `go test` binaries choke on unexpected positional arguments —
// tests substitute a well-behaved fake; production always answers
// os.Executable(), and both sides of the seam assert the same handshake.
var ttyBrokerExecutable = os.Executable

// RunTTYBroker exposes the broker entry to the command layer.
func RunTTYBroker(argv []string, out *os.File) int { return runTTYBrokerFromArgs(argv, out) }

func runTTYBrokerFromArgs(argv []string, out *os.File) int {
	fail := func(format string, args ...any) int {
		b, _ := json.Marshal(ttyHandshake{Err: fmt.Sprintf(format, args...)})
		fmt.Fprintf(out, "%s\n", b)
		out.Close()
		return 1
	}
	opts, cmdArgs, ok := parseBrokerArgs(argv)
	if !ok || len(cmdArgs) == 0 {
		return fail("broker needs --log PATH --sock PATH [--svc NAME] -- then the child argv")
	}

	master, slavePath, err := openPty()
	if err != nil {
		return fail("%v", err)
	}
	defer master.Close()

	logFile, err := os.OpenFile(opts.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fail("open service log %s: %v", opts.logPath, err)
	}
	defer logFile.Close()

	slave, err := os.OpenFile(slavePath, os.O_RDWR, 0)
	if err != nil {
		return fail("open pty slave %s: %v", slavePath, err)
	}

	child := exec.Command(cmdArgs[0], cmdArgs[1:]...) // #nosec G204 -- declared in mabo-ctl.yaml; same trust boundary as every spawn
	child.Stdin = slave
	child.Stdout = slave
	child.Stderr = slave
	child.SysProcAttr = detachAttr()
	child.SysProcAttr.Setctty = true // fd 0 in the CHILD is the slave
	child.SysProcAttr.Ctty = 0
	startedAt := time.Now()
	if serr := child.Start(); serr != nil {
		return fail("spawn %s: %v", cmdArgs[0], serr)
	}
	slave.Close() // the child owns its end; the broker keeps only the master

	hub := newTeeHub(logFile, master)
	go pumpMasterToHub(master, hub)

	sockPath, svc := opts.sockPath, opts.svc
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
		return fail("listen %s: %v", sockPath, err)
	}
	_ = os.Chmod(sockPath, 0o600)

	b, _ := json.Marshal(ttyHandshake{PID: child.Process.Pid})
	fmt.Fprintf(out, "%s\n", b) // THE handshake line: the parent records this pid

	st, stErr := state.New(rootOfSocket(sockPath))
	go serveAttach(ln, hub)

	werr := child.Wait()
	rec := state.ExitRecord{PID: child.Process.Pid, StartedAt: startedAt, EndedAt: time.Now()}
	var ee *exec.ExitError
	switch {
	case errors.As(werr, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			rec.ExitCode = -1
			rec.Signal = syscall.Signal(ws.Signal()).String()
		} else {
			rec.ExitCode = ee.ExitCode()
		}
	case werr == nil:
		rec.ExitCode = 0
	default:
		rec.ExitCode = -1
	}
	rec.LogTail = hub.tailSnapshot()
	if stErr == nil {
		_ = st.WriteExit(svc, rec)
	}

	ln.Close()
	os.Remove(sockPath)
	out.Close()
	return 0
}

type brokerOpts struct{ logPath, sockPath, svc string }

// parseBrokerArgs splits `--log P --sock P [--svc NAME] -- ARGV...`. The
// service name defaults to the socket's base name minus .sock, keeping the
// parent's argv stable even though the name is redundant with the path.
func parseBrokerArgs(argv []string) (o brokerOpts, rest []string, ok bool) {
	for len(argv) > 0 && strings.HasPrefix(argv[0], "--") && argv[0] != "--" {
		if len(argv) < 2 {
			return o, nil, false
		}
		switch argv[0] {
		case "--log":
			o.logPath = argv[1]
		case "--sock":
			o.sockPath = argv[1]
		case "--svc":
			o.svc = argv[1]
		default:
			return o, nil, false
		}
		argv = argv[2:]
	}
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if o.svc == "" && o.sockPath != "" {
		o.svc = strings.TrimSuffix(filepath.Base(o.sockPath), ".sock")
	}
	if o.logPath == "" || o.sockPath == "" {
		return o, nil, false
	}
	return o, argv, true
}

// rootOfSocket reverses TTYSockPath: <root>/.dev/tty/<svc>.sock -> <root>.
func rootOfSocket(sockPath string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(sockPath)))
}

// teeHub fans the single master reader out to the log file plus AT MOST ONE
// attached client, and keeps a bounded snapshot for the exit record.
type teeHub struct {
	mu      sync.Mutex
	log     io.Writer
	slave   io.Writer
	client  net.Conn
	tail    []byte
	busyMsg []byte
}

func newTeeHub(logFile io.Writer, slave io.Writer) *teeHub {
	h := &teeHub{
		log:     logFile,
		slave:   slave,
		busyMsg: []byte("mabo-ctl: another terminal is attached to this service\r\n"),
	}
	return h
}

// write sends one master chunk to the log, any attached client, and history,
// under the one lock all three readers of state share.
func (h *teeHub) write(p []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, _ = h.log.Write(p)
	if h.client != nil {
		_, _ = h.client.Write(p)
	}
	h.tail = append(h.tail, p...)
	if len(h.tail) > ttyBrokerLogTailBytes {
		h.tail = h.tail[len(h.tail)-ttyBrokerLogTailBytes:]
	}
}

// attach claims the client slot, refusing this conn when someone holds it —
// silence would read as a broken pane, not a busy one.
func (h *teeHub) attach(c net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		_, _ = c.Write(h.busyMsg)
		c.Close()
		return false
	}
	h.client = c
	return true
}

func (h *teeHub) detach(c net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client == c {
		h.client = nil
	}
}

// tailSnapshot trims the kept output to begin on a newline where possible, so
// a quoted stack trace starts somewhere readable.
func (h *teeHub) tailSnapshot() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := string(h.tail)
	if i := strings.IndexByte(s, '\n'); i >= 0 && len(s)-i-1 > 0 {
		return s[i+1:]
	}
	return s
}

func pumpMasterToHub(master *os.File, hub *teeHub) {
	buf := make([]byte, 32*1024)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			hub.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// serveAttach accepts relay clients until the listener closes: one live
// session at a time, its client->master half filtered by the detach key.
func serveAttach(ln net.Listener, hub *teeHub) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if !hub.attach(conn) {
			continue
		}
		client, slaveW := conn, hub.slave
		go func() {
			defer func() {
				hub.detach(client)
				client.Close()
			}()
			buf := make([]byte, 32*1024)
			for {
				n, rerr := client.Read(buf)
				for i := 0; i < n; i++ {
					if buf[i] == ttyDetachByte {
						return
					}
				}
				if n > 0 && slaveW != nil {
					if _, werr := slaveW.Write(buf[:n]); werr != nil {
						return
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
	}
}

// bufioReadLine reads one handshake line without pulling in extra framing.
func bufioReadLine(r io.Reader) ([]byte, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return nil, err
	}
	return []byte(strings.TrimRight(line, "\r\n")), nil
}
