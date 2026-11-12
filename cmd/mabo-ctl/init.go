package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/maborak/mabo-ctl/internal/config"
)

// initCmd builds `mabo-ctl init`.
//
// It is deliberately exempt from config loading — like --help and completion —
// because its whole job is to exist where no config can be found yet.
func (a *app) initCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold a commented-out mabo-ctl.yaml from what this repo looks like",
		Long: `Init writes a mabo-ctl.yaml in the current directory, scaffolded from
detection in this directory and its immediate children: a package.json with a
dev script, a .nvmrc beside it, a manage.py, a pyproject.toml, a Cargo.toml.

EVERY guess lands as a commented-out line with the evidence that produced it,
so nothing runs until a human uncomments it — a generated cmd that is subtly
wrong is worse than a blank template. Init writes the file, adds .mabo-ctl/ to
.gitignore, and exits. It never runs a build, an install step, or anything the
detection found.

--from imports another tool's process file instead of detecting: a Procfile
(foreman, overmind, honcho) or a process-compose.yaml. Those are commands you
already run, so they are written LIVE, each through ` + "`sh -c`" + `; a Procfile
process that reads $PORT gets foreman's port (5000, +100 per process), and a
process-compose http_get readiness probe becomes the health URL. Anything that
cannot be carried over safely is written commented out with the reason, and
the result is validated before init reports success.

  mabo-ctl init                       detect, everything commented out
  mabo-ctl init --from Procfile       import a Procfile
  mabo-ctl init --from process-compose.yaml`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, _ := cmd.Flags().GetString("from")
			return a.runInit(from)
		},
	}
	cmd.Flags().String("from", "", "import a Procfile or process-compose.yaml instead of detecting; written live, not commented out")
	return cmd
}

// runInit scaffolds the file: refuse to overwrite, detect, write, fix
// .gitignore, report.
func (a *app) runInit(from string) error {
	wd := a.env.Wd
	if wd == "" {
		var err error
		if wd, err = os.Getwd(); err != nil {
			return fmt.Errorf("mabo-ctl init: resolve working directory: %w", err)
		}
	}

	target := filepath.Join(wd, config.FileName)
	if _, err := os.Stat(target); err == nil {
		return usageErrorf("mabo-ctl init: %s already exists; refusing to overwrite it", target)
	}
	legacy := filepath.Join(wd, config.LegacyFileName)
	if _, err := os.Stat(legacy); err == nil {
		return usageErrorf("mabo-ctl init: %s already exists under its old name; rename it to %s instead of scaffolding a second file",
			config.LegacyFileName, config.FileName)
	}

	if from != "" {
		return a.runInitImport(wd, target, from)
	}

	guesses := detectServices(wd)
	body := renderInit(guesses)
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		return fmt.Errorf("mabo-ctl init: write %s: %w", target, err)
	}

	gitignored, err := ensureGitIgnore(filepath.Join(wd, ".gitignore"))
	if err != nil {
		fmt.Fprintf(a.env.Stderr, "mabo-ctl init: %v\n", err)
	}

	fmt.Fprintf(a.env.Stdout, "wrote %s\n", target)
	fmt.Fprintf(a.env.Stdout, "%d service guess(es), all commented out — edit, then run `mabo-ctl preflight`\n", len(guesses))
	if gitignored {
		fmt.Fprintln(a.env.Stdout, ".mabo-ctl/ added to .gitignore")
	}
	return nil
}

// runInitImport writes mabo-ctl.yaml from another tool's process file, then
// loads it back through the real validator, so a file init calls good is one
// every other command accepts.
func (a *app) runInitImport(wd, target, from string) error {
	if !filepath.IsAbs(from) {
		from = filepath.Join(wd, from)
	}
	svcs, kind, err := importServices(from)
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		return fmt.Errorf("mabo-ctl init --from: %s declares no processes", from)
	}
	// The header names the source relative to the repo: the file is meant to
	// be committed, and an absolute path would publish the author's home.
	shown := filepath.Base(from)
	if rel, err := filepath.Rel(wd, from); err == nil && !strings.HasPrefix(rel, "..") {
		shown = rel
	}
	if err := os.WriteFile(target, []byte(renderImported(svcs, kind, shown)), 0o644); err != nil {
		return fmt.Errorf("mabo-ctl init: write %s: %w", target, err)
	}
	gitignored, gerr := ensureGitIgnore(filepath.Join(wd, ".gitignore"))
	if gerr != nil {
		fmt.Fprintf(a.env.Stderr, "mabo-ctl init: %v\n", gerr)
	}
	fmt.Fprintf(a.env.Stdout, "wrote %s from %s (%d process(es))\n", target, from, len(svcs))
	if gitignored {
		fmt.Fprintln(a.env.Stdout, ".mabo-ctl/ added to .gitignore")
	}
	if _, err := config.Load(target); err != nil {
		return fmt.Errorf("mabo-ctl init --from: the imported file does not validate yet; edit %s and run `mabo-ctl config`:\n%w", target, err)
	}
	fmt.Fprintln(a.env.Stdout, "it validates — review the TODO comments, then `mabo-ctl preflight` and `mabo-ctl start`")
	return nil
}

// guess is one detected candidate service. Lines are the YAML fragment for it,
// EVERY LINE PRE-COMMENTED, plus an evidence comment saying why the guess was
// made — so the reader adjudicates instead of trusting.
type guess struct {
	name    string
	comment string
	lines   []string
}

// detectServices looks at wd itself and each immediate subdirectory for the
// shapes a dev service takes. Detection reads files; it runs nothing.
func detectServices(wd string) []guess {
	var out []guess

	if g := detectNode(wd, filepath.Base(wd)); g != nil {
		out = append(out, *g)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(wd, e.Name())
		if g := detectNode(dir, e.Name()); g != nil {
			out = append(out, *g)
			continue
		}
		if g := detectPython(dir, e.Name()); g != nil {
			out = append(out, *g)
			continue
		}
		if g := detectRust(dir, e.Name()); g != nil {
			out = append(out, *g)
		}
	}
	return out
}

// detectNode recognises a package.json with a dev script, optionally versioned
// by a .nvmrc sitting beside it.
func detectNode(dir, name string) *guess {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil || pkg.Scripts["dev"] == "" {
		return nil
	}

	g := &guess{name: sanitizeName(name), comment: fmt.Sprintf(
		"%s: package.json declares a dev script (%q)", name, pkg.Scripts["dev"])}
	g.lines = append(g.lines, "# - name: "+g.name)
	if rc, err := os.ReadFile(filepath.Join(dir, ".nvmrc")); err == nil {
		if v := nvmrcVersion(string(rc)); v != "" {
			g.lines = append(g.lines,
				"#   runtime: node:"+v+"  # from .nvmrc")
		}
	}
	g.lines = append(g.lines,
		"#   cmd: [npm, run, dev]",
		"#   # port: 3000  # TODO: whatever the dev script actually binds",
		"#   health: http://localhost:3000/  # TODO: any HTTP response counts as ready;",
		"#         # the origin / is fine, robots.txt famously lies",
	)
	return g
}

// detectPython recognises a Django manage.py or a Python project file, in that
// order of specificity.
func detectPython(dir, name string) *guess {
	switch {
	case fileExists(filepath.Join(dir, "manage.py")):
		return &guess{
			name:    sanitizeName(name),
			comment: name + ": manage.py found (Django)",
			lines: []string{
				"# - name: " + sanitizeName(name),
				"#   cmd: [python, manage.py, runserver]",
				"#   # port: 8000  # TODO: runserver defaults to 8000",
				"#   health: http://localhost:8000/  # TODO: any HTTP response counts",
			},
		}
	case fileExists(filepath.Join(dir, "pyproject.toml")):
		return &guess{
			name:    sanitizeName(name),
			comment: name + ": pyproject.toml found; if this is a FastAPI/uvicorn app:",
			lines: []string{
				"# - name: " + sanitizeName(name),
				"#   cmd: [uvicorn, app.main:app, --reload]",
				"#   # port: 8000  # TODO: confirm the app module path and port",
				"#   health: http://localhost:8000/  # TODO: any HTTP response counts",
			},
		}
	default:
		return nil
	}
}

// detectRust recognises a Cargo.toml.
func detectRust(dir, name string) *guess {
	if !fileExists(filepath.Join(dir, "Cargo.toml")) {
		return nil
	}
	return &guess{
		name:    sanitizeName(name),
		comment: name + ": Cargo.toml found",
		lines: []string{
			"# - name: " + sanitizeName(name),
			"#   cmd: [cargo, run]",
			"#   # port: 8080  # TODO: only if the binary serves HTTP",
		},
	}
}

// renderInit assembles the whole document: instructions on top, every guess
// commented out underneath, and a pointer at the annotated example.
func renderInit(guesses []guess) string {
	var b strings.Builder
	b.WriteString("# mabo-ctl.yaml — scaffolded by `mabo-ctl init`. NOTHING HERE RUNS YET:\n")
	b.WriteString("# every service below is a comment. Uncomment and edit, then\n")
	b.WriteString("# `mabo-ctl preflight` checks the machine against what you kept.\n#\n")
	b.WriteString("# Annotated reference: examples/mabo-ctl.yaml in the mabo-ctl repository.\n")

	if len(guesses) == 0 {
		b.WriteString("\n# No service shapes were recognised in this directory tree.\n")
		b.WriteString("# Start from this skeleton:\n#\n")
		b.WriteString("# services:\n")
		b.WriteString("#   - name: app\n")
		b.WriteString("#     cmd: [echo, edit-me]\n")
		b.WriteString("#     # port: 8000\n")
		b.WriteString("#     health: http://localhost:8000/  # any HTTP response counts as ready;\n")
		b.WriteString("#           # the origin / is fine, robots.txt famously lies\n")
		return b.String()
	}

	b.WriteString("\nservices: []  # TODO: replace this line with the uncommented entries below\n")
	for _, g := range guesses {
		fmt.Fprintf(&b, "\n  # %s\n", yamlComment(g.comment))
		for _, l := range g.lines {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	return b.String()
}

// yamlComment makes repo-derived text safe inside a '#' comment line. A
// newline in a directory name, a package.json field or any other detected
// file would otherwise terminate the comment and let the rest of the line
// become ACTIVE yaml in the scaffolded file — and a scaffold's checks:
// entries execute when preflight runs. Every control character becomes a
// space, C1 included, and so do the line breaks YAML recognises beyond ASCII:
// NEL (U+0085), LINE SEPARATOR (U+2028) and PARAGRAPH SEPARATOR (U+2029).
// The comment stays readable and stays a comment.
func yamlComment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
}

// nvmrcVersion reduces an .nvmrc value to the characters a `runtime: node:`
// declaration can legally carry: versions, "lts/iron" style names, nothing
// else. The raw file is untrusted repo content, and TrimSpace alone would
// keep an internal newline — a second, UNCOMMENTED line of attacker yaml in
// the scaffold. The leading "v" is kept as written; the runtime parser
// accepts both forms.
func nvmrcVersion(rc string) string {
	rc = strings.TrimSpace(rc)
	var b strings.Builder
	for _, r := range rc {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '/':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitizeName maps a directory name onto a legal service name: the rule that
// guards .mabo-ctl/logs/<name>.log is applied even to suggestions, so uncommenting
// can never paste an illegal name into the file.
func sanitizeName(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case (c == '_' || c == '-') && b.Len() > 0:
			b.WriteByte(c)
		}
	}
	out := b.String()
	if out == "" {
		return "app"
	}
	return out
}

// ensureGitIgnore adds .mabo-ctl/ to path, creating the file when missing. It reports
// whether it changed anything; a failure is returned rather than fatal, because
// a scaffolded config is still a success without the ignore rule.
func ensureGitIgnore(path string) (bool, error) {
	existing, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("read %s: %w", path, err)
		}
		if werr := os.WriteFile(path, []byte(".mabo-ctl/\n"), 0o644); werr != nil {
			return false, fmt.Errorf("create %s: %w", path, werr)
		}
		return true, nil
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == ".mabo-ctl/" {
			return false, nil // already there; say nothing
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, fmt.Errorf("append to %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		fmt.Fprintln(f)
	}
	if _, err := fmt.Fprintln(f, ".mabo-ctl/"); err != nil {
		return false, fmt.Errorf("append to %s: %w", path, err)
	}
	return true, nil
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
