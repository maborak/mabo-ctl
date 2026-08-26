//go:build darwin

package supervisor

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// openPty allocates a fresh pseudo-terminal pair on darwin. macOS grants
// ptys via libSystem's openpty(3); every stdlib-only route that reopens the
// granted /dev/ttysN inside this same process answers EAGAIN indefinitely
// (verified empirically against /dev/ptmx + TIOCPTYGRANT), so rather than
// half-work, the platform declines until that primitive has a home here.
func openPty() (*os.File, string, error) {
	return nil, "", errors.New("tty terminals are not yet supported on darwin" +
		" (no libc-free openpty); declare tty: false and use mabo-ctl exec instead")
}

// makeControllingTTY makes fd the controlling terminal of the calling process,
// which must already be a session leader. Without it a shell child cannot take
// the terminal for its own job control.
func makeControllingTTY(fd uintptr) error {
	return ioctlPtr(fd, syscall.TIOCSCTTY, nil)
}

// termiosFor reads and makeRaw applies cfmakeraw semantics to fd's terminal —
// the attach client's side of the relay, so keystrokes arrive at the broker one
// byte each and nothing is echoed twice.
func termiosGet(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctlPtr(fd, syscall.TIOCGETA, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	return &t, nil
}

func termiosSet(fd uintptr, t *syscall.Termios) error {
	return ioctlPtr(fd, syscall.TIOCSETA, unsafe.Pointer(t))
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
