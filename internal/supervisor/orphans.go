package supervisor

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// leaderlessGroup decides whether the live process group pgid, whose leader —
// the pid this service's record names — has already exited, is still this
// service's, and returns its members when it is.
//
// The leader is what verifyGroup checks a pid record against, so with it gone
// there is no start time to compare. What makes a reused group possible at all
// is narrow: the kernel never hands out a pid that is still in use as a group
// id, so pgid can only belong to someone else if this service's group emptied
// completely and the number was recycled into a new group, such as one a
// double-forking daemon leaves behind. Start times cannot tell those apart, so
// the proof is a tie to this checkout instead: at least one member holds the
// service's declared port, or works in a directory inside the checkout. With
// neither, the group is not signalled and the error names its members.
func (s *Supervisor) leaderlessGroup(name string, pgid int) ([]int, error) {
	members, err := groupMembers(pgid)
	if err != nil {
		return nil, fmt.Errorf("%w: pid %d has exited and its process group %d is still alive, "+
			"but its members could not be listed: %v", ErrUnsafeSignal, pgid, pgid, err)
	}
	if len(members) == 0 {
		return nil, nil
	}
	if in, ok := s.byName[name]; ok && in.Port > 0 {
		if h := PortHolder(in.Port); h.PID > 0 && slices.Contains(members, h.PID) {
			return members, nil
		}
	}
	root := realPath(s.st.Root)
	for _, cwd := range processCwds(members) {
		if within(realPath(cwd), root) {
			return members, nil
		}
	}
	return nil, fmt.Errorf("%w: pid %d has exited, and nothing ties the processes still in its group "+
		"(pids %s) to this service: none holds its port or works inside %s. The group id may have "+
		"been reused; inspect them with: ps -o pid,pgid,command -g %d",
		ErrUnsafeSignal, pgid, joinPIDs(members), s.st.Root, pgid)
}

// realPath resolves symlinks so two spellings of one directory compare equal:
// on macOS a temp dir is /var/… to Go and /private/var/… to lsof.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// within reports whether path is root or lies below it.
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// joinPIDs renders pids as "1, 2, 3".
func joinPIDs(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ", ")
}
