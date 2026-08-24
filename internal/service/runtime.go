package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// runtimeArgRE is the set of legal conda environment names and node versions.
// The value composes a filesystem path, so a "/" or ".." in it would escape the
// runtime root. config enforces the same rule at load time; it is enforced again
// here because Resolve accepts any *config.Config.
var runtimeArgRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// condaHomeDirs are the conda installations to look for under the user's home
// directory, in order, when the environment names none.
var condaHomeDirs = []string{"miniconda3", "anaconda3"}

// errRuntimeUnavailable marks a runtime failure that is a property of THIS
// MACHINE rather than of mabo-ctl.yaml: the interpreter, the conda base or the
// nvm directory is not installed here.
//
// The distinction decides whether the failure is fatal. A MALFORMED runtime
// declaration ("docker:latest", "conda:../..") is wrong on every machine, so it
// fails the whole load. A missing interpreter is wrong only here, so it is
// deferred onto Instance.CmdErr and fails just that service when something
// tries to run it — otherwise a developer without conda installed could not
// stop, inspect or reset the services they already have running.
var errRuntimeUnavailable = errors.New("runtime unavailable on this machine")

// unavailableError tags a runtime error as [errRuntimeUnavailable] without
// altering its message, so the text a user reads still leads with the resolved
// path that was tried rather than with a category name.
type unavailableError struct{ err error }

// Error returns the wrapped error's message unchanged.
func (e unavailableError) Error() string { return e.err.Error() }

// Unwrap exposes both the original error and [errRuntimeUnavailable], so
// errors.Is matches either.
func (e unavailableError) Unwrap() []error { return []error{e.err, errRuntimeUnavailable} }

// unavailable marks err as an environment-specific runtime failure.
func unavailable(err error) error { return unavailableError{err} }

// resolvedRuntime is the outcome of resolving a service's Cmd[0].
type resolvedRuntime struct {
	// Path is the absolute path of the executable to run.
	Path string
	// BinDir is the directory to prepend to the child's PATH, or "" for the
	// system runtime.
	BinDir string
	// Env holds extra variables the runtime needs in the child environment.
	Env map[string]string
}

// resolveRuntime turns cmd0 into an absolute executable path according to the
// declared runtime. This is where dev.sh bug #5 dies: the shell version
// inherited the caller's PATH, so under a non-login shell nvm was absent and
// `npm` was missing or the wrong major version.
//
// The rules:
//
//   - "" or "system" — cmd0 is looked up in searchPath, or, when it contains a
//     path separator, resolved relative to dir.
//   - "conda:<env>" — <conda_base>/envs/<env>/bin/<cmd0>, where conda_base is
//     the first of $CONDA_EXE's installation, $CONDA_PREFIX's installation,
//     ~/miniconda3 or ~/anaconda3 that exists.
//   - "node:<version>" — $NVM_DIR/versions/node/v<version>/bin/<cmd0>, with
//     NVM_DIR defaulting to ~/.nvm.
//
// There is no fallback to an ambient PATH lookup: a service that declares a
// runtime runs that runtime's interpreter or it does not run. Every failure
// names the RESOLVED PATH that was tried and the runtime that produced it.
//
// A non-system runtime also contributes its bin directory to the child's PATH.
// That is not a convenience: `npm` is a script whose shebang re-resolves `node`
// through PATH, so running the right npm with the wrong PATH still runs the
// wrong node.
func resolveRuntime(svc, runtime, cmd0, dir, searchPath string) (resolvedRuntime, error) {
	if err := checkPlatform(); err != nil {
		return resolvedRuntime{}, fmt.Errorf("service %q: %w", svc, err)
	}

	kind, arg, err := splitRuntime(runtime)
	if err != nil {
		return resolvedRuntime{}, fmt.Errorf("service %q: %w", svc, err)
	}

	switch kind {
	case "system":
		path, err := resolveSystem(cmd0, dir, searchPath)
		if err != nil {
			return resolvedRuntime{}, unavailable(
				fmt.Errorf("service %q: runtime %q: %w", svc, displayRuntime(runtime), err))
		}
		return resolvedRuntime{Path: path}, nil

	case "conda":
		base, err := condaBase()
		if err != nil {
			return resolvedRuntime{}, unavailable(
				fmt.Errorf("service %q: runtime %q: %w", svc, runtime, err))
		}
		prefix := filepath.Join(base, "envs", arg)
		bin := filepath.Join(prefix, "bin")
		path := filepath.Join(bin, cmd0)
		if err := executableFile(path); err != nil {
			return resolvedRuntime{}, unavailable(fmt.Errorf(
				"service %q: runtime %q resolves %q to %s, and %w (conda base %s); "+
					"mabo-ctl never falls back to PATH for a declared runtime",
				svc, runtime, cmd0, path, err, base))
		}
		return resolvedRuntime{
			Path:   path,
			BinDir: bin,
			Env: map[string]string{
				"CONDA_PREFIX":      prefix,
				"CONDA_DEFAULT_ENV": arg,
			},
		}, nil

	case "node":
		nvm, err := nvmDir()
		if err != nil {
			return resolvedRuntime{}, unavailable(
				fmt.Errorf("service %q: runtime %q: %w", svc, runtime, err))
		}
		version := "v" + strings.TrimPrefix(arg, "v")
		bin := filepath.Join(nvm, "versions", "node", version, "bin")
		path := filepath.Join(bin, cmd0)
		if err := executableFile(path); err != nil {
			return resolvedRuntime{}, unavailable(fmt.Errorf(
				"service %q: runtime %q resolves %q to %s, and %w (nvm dir %s); "+
					"mabo-ctl never falls back to PATH for a declared runtime",
				svc, runtime, cmd0, path, err, nvm))
		}
		return resolvedRuntime{Path: path, BinDir: bin}, nil
	}

	// splitRuntime rejects everything else, so this is unreachable.
	return resolvedRuntime{}, fmt.Errorf("service %q: unhandled runtime %q", svc, runtime)
}

// splitRuntime parses a runtime declaration into a kind ("system", "conda" or
// "node") and its argument, which is empty for "system".
func splitRuntime(runtime string) (kind, arg string, err error) {
	switch strings.TrimSpace(runtime) {
	case "", "system":
		return "system", "", nil
	}
	kind, arg, ok := strings.Cut(runtime, ":")
	if !ok || (kind != "conda" && kind != "node") {
		return "", "", fmt.Errorf(
			"invalid runtime %q; must be \"\", \"system\", \"conda:<env>\" or \"node:<version>\"", runtime)
	}
	if !runtimeArgRE.MatchString(arg) {
		what := "conda environment name"
		if kind == "node" {
			what = "node version"
		}
		return "", "", fmt.Errorf(
			"invalid runtime %q: the %s must match %s; it composes a filesystem path, so %q or %q in it would escape the runtime root",
			runtime, what, runtimeArgRE.String(), "/", "..")
	}
	return kind, arg, nil
}

// displayRuntime renders an empty runtime as "system" for messages.
func displayRuntime(runtime string) string {
	if strings.TrimSpace(runtime) == "" {
		return "system"
	}
	return runtime
}

// resolveSystem resolves cmd0 for the system runtime. A cmd0 containing a path
// separator is taken as a path relative to the service's own directory (the
// child's working directory) unless it is already absolute; otherwise it is
// searched for in searchPath, which is the PATH the CHILD will run with, not
// necessarily mabo-ctl's own.
//
// An empty element in searchPath is skipped rather than treated as the current
// directory: resolving an interpreter out of whatever directory mabo-ctl happens
// to be in is not a behaviour worth supporting.
func resolveSystem(cmd0, dir, searchPath string) (string, error) {
	if strings.ContainsRune(cmd0, os.PathSeparator) {
		path := cmd0
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		path = filepath.Clean(path)
		if err := executableFile(path); err != nil {
			return "", fmt.Errorf("%q resolves to %s, and %w", cmd0, path, err)
		}
		return path, nil
	}

	var tried []string
	for _, elem := range filepath.SplitList(searchPath) {
		if strings.TrimSpace(elem) == "" {
			continue
		}
		if !filepath.IsAbs(elem) {
			elem = filepath.Join(dir, elem)
		}
		path := filepath.Join(elem, cmd0)
		if err := executableFile(path); err == nil {
			return path, nil
		}
		tried = append(tried, path)
	}
	if len(tried) == 0 {
		return "", fmt.Errorf("%q was not found: PATH is empty, so there is nowhere to look", cmd0)
	}
	return "", fmt.Errorf("%q was not found as an executable file in any PATH directory; tried: %s",
		cmd0, strings.Join(tried, ", "))
}

// condaBase returns the conda installation root: the first of $CONDA_EXE's
// installation (CONDA_EXE is <base>/bin/conda), $CONDA_PREFIX's installation,
// ~/miniconda3 or ~/anaconda3 that exists as a directory.
//
// $CONDA_PREFIX points at the ACTIVE environment, which under an activated env
// is <base>/envs/<name>; that suffix is stripped so the base is recovered rather
// than an envs/<name>/envs/<name> path being built later.
//
// The error names every candidate that was tried.
func condaBase() (string, error) {
	type candidate struct {
		path string
		from string
	}
	var candidates []candidate

	if exe := strings.TrimSpace(os.Getenv("CONDA_EXE")); exe != "" {
		candidates = append(candidates, candidate{filepath.Dir(filepath.Dir(exe)), "$CONDA_EXE=" + exe})
	}
	if prefix := strings.TrimSpace(os.Getenv("CONDA_PREFIX")); prefix != "" {
		base := prefix
		if filepath.Base(filepath.Dir(base)) == "envs" {
			base = filepath.Dir(filepath.Dir(base))
		}
		candidates = append(candidates, candidate{base, "$CONDA_PREFIX=" + prefix})
	}
	home, homeErr := os.UserHomeDir()
	if homeErr == nil {
		for _, name := range condaHomeDirs {
			p := filepath.Join(home, name)
			candidates = append(candidates, candidate{p, "~/" + name})
		}
	}

	var tried []string
	for _, c := range candidates {
		if isDir(c.path) {
			return c.path, nil
		}
		tried = append(tried, fmt.Sprintf("%s (%s)", c.path, c.from))
	}
	if len(tried) == 0 {
		return "", errors.New("no conda installation found: $CONDA_EXE and $CONDA_PREFIX are unset and the home directory could not be determined")
	}
	return "", fmt.Errorf("no conda installation found; tried: %s", strings.Join(tried, ", "))
}

// nvmDir returns $NVM_DIR, or ~/.nvm when it is unset. It does not require the
// directory to exist: the caller's error is more useful when it names the full
// interpreter path that was missing.
func nvmDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("NVM_DIR")); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("$NVM_DIR is unset and the home directory could not be determined: %w", err)
	}
	return filepath.Join(home, ".nvm"), nil
}

// executableFile reports why path cannot be executed, or nil when it can. The
// returned errors are phrased to read after "..., and": "does not exist",
// "is a directory", "is not executable".
func executableFile(path string) error {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("that file does not exist")
	}
	if err != nil {
		return fmt.Errorf("that file cannot be inspected: %w", err)
	}
	if fi.IsDir() {
		return errors.New("that path is a directory, not a program")
	}
	if !executableMode(fi.Mode()) {
		return fmt.Errorf("that file is not executable (mode %s)", fi.Mode().Perm())
	}
	return nil
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
