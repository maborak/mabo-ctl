package main

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
)

// Build stamps, set with -ldflags at link time by the Makefile:
//
//	go build -ldflags '-X main.version=v1.2.3 -X main.commit=abc1234 -X main.builtAt=2026-08-26T00:00:00Z'
//
// They are plain vars rather than consts because the linker's -X can only
// rewrite a variable. The defaults are what an unstamped `go build ./...`
// produces, and they are only the START of what the binary knows about
// itself: since Go 1.18 the toolchain embeds the VCS state in every build it
// makes from a git checkout, so [theBuild] fills each unstamped gap from
// there and "dev (commit unknown)" becomes "dev (commit 1a2b3c…-dirty)" with
// no stamping machinery involved.
var (
	// version is the release the binary was built from. Left as the literal
	// fallback "dev" even when a VCS revision is available, on purpose: an
	// upgrade can never mistake a source build for a release tag, which is
	// exactly the mistake selfupdate.Compare refuses to guess at.
	version = "dev"
	// commit is the git revision the binary was built from.
	commit = "unknown"
	// builtAt is when the binary was linked, UTC.
	builtAt = "unknown"
)

// buildInfo is everything the running binary knows about its own provenance:
// the version stamp, commit, dirty flag, build time, the toolchain that
// compiled it, the platform, cgo mode, module identity, every build setting
// the compiler recorded and every dependency link pulled in. It backs both
// versions of the self-report — [buildInfo.Summary], one line for help text,
// and [buildInfo.Report], the complete block `--version` prints. A bug report
// pastes from `--version`, so completeness matters more than brevity there;
// honesty matters more than either everywhere.
type buildInfo struct {
	Version  string
	Commit   string
	Dirty    bool
	BuiltAt  string
	Go       string
	Platform string
	CGO      string
	Module   string

	Settings []debug.BuildSetting // verbatim, including vcs.* keys
	Deps     []string             // "path@version", sorted by the toolchain
}

// theBuild resolves the running binary's build information at most once per
// process. Callers construct commands and render output against this value
// rather than reading the raw stamp vars, so a plain build reports what its
// toolchain knew instead of the linker's defaults.
var theBuild = sync.OnceValue(func() buildInfo {
	bi, _ := debug.ReadBuildInfo()
	return resolveBuild(version, commit, builtAt, bi)
})

// resolveBuild merges the ldflags stamps over the embedded build information
// given as bi, which may be nil when the toolchain had none (a very old Go,
// or a stripped binary run outside any module context). The stamps win
// wherever they say something: they are set deliberately by the Makefile
// (and may disagree with the working tree at build time, e.g. a dist target
// built from a pinned tarball). Every field left as the default sentinel
// falls back to the embedded VCS metadata when there is one.
func resolveBuild(stampedVersion, stampedCommit, stampedBuiltAt string, bi *debug.BuildInfo) buildInfo {
	b := buildInfo{
		Version:  stampedVersion,
		Commit:   stampedCommit,
		BuiltAt:  stampedBuiltAt,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Commit == "" {
		b.Commit = "unknown"
	}
	if b.BuiltAt == "" {
		b.BuiltAt = "unknown"
	}

	if bi == nil {
		b.CGO = "unknown"
		return b
	}

	var rev, modified, when, cgo string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		case "vcs.time":
			when = s.Value
		case "CGO_ENABLED":
			cgo = s.Value
		}
	}
	if cgo == "" {
		b.CGO = "unknown"
	} else {
		b.CGO = cgoLabel(cgo)
	}
	b.Dirty = modified == "true"

	// Deliberately NOT deriving b.Version from rev: see the var block's
	// comment — version stays exactly what was stamped.
	if unstamped(b.Commit, "unknown") && rev != "" {
		b.Commit = shortRevision(rev)
	}
	if unstamped(b.BuiltAt, "unknown") && when != "" {
		b.BuiltAt = when
	}
	b.Module = moduleLine(bi)
	b.Settings = append(b.Settings, bi.Settings...)
	for _, d := range bi.Deps {
		b.Deps = append(b.Deps, depLine(d))
	}
	return b
}

// moduleLine renders the main module's identity: path plus recorded version,
// "(devel)" being what the toolchain writes for a build outside a release.
func moduleLine(bi *debug.BuildInfo) string {
	m := bi.Main
	v := m.Version
	if v == "" || v == "(devel)" {
		return m.Path + " (devel)"
	}
	return m.Path + " " + v
}

// depLine renders one dependency as path@version, noting replacements
// because a replaced dependency is frequently the interesting fact.
func depLine(d *debug.Module) string {
	line := d.Path + "@" + d.Version
	if d.Replace != nil {
		line += " (replaced by " + d.Replace.Path + "@" + d.Replace.Version + ")"
	}
	return line
}

// cgoLabel renders the CGO_ENABLED setting as a word.
func cgoLabel(v string) string {
	switch v {
	case "1":
		return "enabled"
	case "0":
		return "disabled"
	default:
		return "unknown"
	}
}

// unstamped reports whether v still holds its default sentinel, where empty
// also counts: neither case says anything the VCS metadata cannot say
// better. A real tag, sha or timestamp wins over that metadata.
func unstamped(v, sentinel string) bool { return v == "" || v == sentinel }

// shortRevision truncates a full VCS revision to twelve hex characters,
// which is how git abbreviates shas by convention and plenty to identify a
// revision in practice.
func shortRevision(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// Summary renders the one-line build stamp: version, commit, dirty marker,
// build time, toolchain and platform. It is the line that used to be
// "dev (commit unknown)" regardless of everything the binary actually knew.
func (b buildInfo) Summary() string {
	c := b.Commit
	if b.Dirty {
		c += "-dirty"
	}
	return b.Version + " (commit " + c + ", built " + b.BuiltAt +
		", go " + b.Go + " " + b.Platform + ")"
}

// Report renders the complete self-report `--version` prints: the summary
// line followed by one key/value pair per fact, then every build setting and
// dependency the toolchain embedded. Format is fixed-width two-space indents
// so a terminal pastes cleanly; nothing here is redacted-sensitive because
// none of it came from the user's environment.
func (b buildInfo) Report() string {
	var sb strings.Builder
	sb.WriteString(b.Summary())
	sb.WriteString("\n")

	kv := func(k, v string) {
		sb.WriteString("  ")
		sb.WriteString(k)
		sb.WriteString(": ")
		sb.WriteString(v)
		sb.WriteString("\n")
	}
	dirty := "false"
	if b.Dirty {
		dirty = "true"
	}
	kv("version", b.Version)
	kv("commit", b.Commit)
	kv("dirty", dirty)
	kv("built at", b.BuiltAt)
	kv("go", b.Go)
	kv("platform", b.Platform)
	kv("cgo", b.CGO)
	if b.Module != "" {
		kv("module", b.Module)
	}
	if len(b.Settings) > 0 {
		sb.WriteString("\nbuild settings:\n")
		for _, s := range b.Settings {
			sb.WriteString("  ")
			sb.WriteString(s.Key)
			sb.WriteString("=")
			sb.WriteString(s.Value)
			sb.WriteString("\n")
		}
	}
	if len(b.Deps) > 0 {
		sb.WriteString("\ndependencies:\n")
		for _, d := range b.Deps {
			sb.WriteString("  ")
			sb.WriteString(d)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
