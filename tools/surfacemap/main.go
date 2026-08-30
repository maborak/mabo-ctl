// Command surfacemap regenerates internal/surface/surfaces.json from the
// CURRENTLY BUILT binary. Run it whenever the drift gate fails:
//
//	go run ./tools/surfacemap
//
// and commit the resulting diff — a reviewed one, never a silent CI `-update`.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/maborak/mabo-ctl/internal/surface"
)

func main() {
	out := flag.String("out", filepath.Join("internal", "surface", "surfaces.json"),
		"where the canonical map is written")
	bin := flag.String("bin", "", "path to an already-built mabo-ctl; default builds one into a temp dir")
	flag.Parse()

	if *bin == "" {
		tmp, err := os.MkdirTemp("", "surfacemap-bin")
		if err != nil {
			fatal(err)
		}
		defer func() { _ = os.RemoveAll(tmp) }()
		p := filepath.Join(tmp, "mabo-ctl")
		b, err := surface.ExecGoBuild(p)
		if err != nil {
			fatal(err)
		}
		*bin = p
		fmt.Fprintln(os.Stderr, "built:", string(b))
	}

	m, err := surface.Enumerate(*bin)
	if err != nil {
		fatal(err)
	}
	n := 0
	for _, ids := range m.Sections {
		n += len(ids)
	}
	if err := surface.WriteCanonical(m, *out); err != nil {
		fatal(err)
	}
	fmt.Printf("surfaces: cli=%d config=%d json=%d http=%d total=%d -> %s\n",
		len(m.Sections["cli"]), len(m.Sections["config"]), len(m.Sections["json"]), len(m.Sections["http"]), n, *out)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "surfacemap:", err)
	os.Exit(1)
}
