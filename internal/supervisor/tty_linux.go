//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// openPty allocates a fresh pseudo-terminal pair on Linux: /dev/ptmx is the
// clone device; the unlock ioctl (TIOCSPTLCK = 0) lets the slave be opened and
// TIOCGPTN names it.
func openPty() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	var unlock int32
	if err := ioctlPtr(master.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		_ = master.Close()
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}
	var n int32
	if err := ioctlPtr(master.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		_ = master.Close()
		return nil, "", fmt.Errorf("resolve pty number: %w", err)
	}
	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	return master, slavePath, nil
}

// (The TIOCSCTTY helper a real Linux implementation will need —
// makeControllingTTY — was removed as dead code until that work lands; see
// git history.)

// termiosGet / termiosSet read and write fd's terminal settings; makeRaw
// applies cfmakeraw semantics so the attach client forwards every byte
// untouched and nothing is echoed from both ends of the relay.
func termiosGet(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctlPtr(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func termiosSet(fd uintptr, t *syscall.Termios) error {
	return ioctlPtr(fd, syscall.TCSETS, unsafe.Pointer(t))
}

func makeRaw(t *syscall.Termios) {
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB
	t.Cflag |= syscall.CS8
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
}

func ioctlPtr(fd uintptr, req uint, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(req), uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}
