// Package selfupdate upgrades the running mabo-ctl binary to the latest GitHub
// release of this repository.
//
// The command replaces a binary, so every gate here is load-bearing:
//
//   - The only egress is two caller-initiated GETs — the release metadata and
//     the platform asset — and both URLs are scheme-checked before use.
//   - The asset's sha256, taken from the release's SHA256SUMS file, is verified
//     while streaming. A mismatch aborts before anything but a temp file was
//     written.
//   - The swap is atomic: the download lands in a temp file in the binary's own
//     directory and renames over the running image, which is safe on every OS
//     this tool supports.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the GitHub API root for this repository's releases.
const DefaultBaseURL = "https://api.github.com/repos/maborak/mabo-ctl"

// Release is one downloadable release of mabo-ctl, resolved for the platform
// Options asked about.
type Release struct {
	// Tag is the release's git tag, e.g. "v0.1.0".
	Tag string
	// AssetName is the platform asset the release offers, e.g.
	// "mabo-ctl-darwin-arm64".
	AssetName string
	// AssetURL downloads AssetName.
	AssetURL string
	// SHA256 is the hex digest AssetName must have, from SHA256SUMS.
	SHA256 string
}

// Options carries the seams Latest and Apply run through. The zero value is
// usable and means the real GitHub repository, a client with a timeout, the
// platform the binary runs on, and https enforced on every URL.
type Options struct {
	// BaseURL is the GitHub API root. It exists so tests can point Latest at
	// an httptest server; production never sets it.
	BaseURL string
	// Client sends both requests. Nil means one with a 60s timeout.
	Client *http.Client
	// GOOS and GOARCH select the asset. They exist for the same reason as
	// BaseURL: tests must not depend on the machine running them.
	GOOS   string
	GOARCH string
	// AllowPlainHTTP lifts the https requirement. It exists ONLY so tests can
	// reach an httptest server; production never sets it, and the flag is named
	// so that a grep for "http://" in a security review lands here.
	AllowPlainHTTP bool
}

// assetName is the dist/ file name of the binary for one platform, matching the
// names `make dist` and the release workflow upload.
func assetName(goos, goarch string) string {
	return "mabo-ctl-" + goos + "-" + goarch
}

// Latest resolves the newest published release to the asset for the platform
// opt selects. A repository with no releases, an asset for a platform the
// release does not ship, or a missing SHA256SUMS are errors, not empty
// results: an upgrade that cannot be verified must not look like one that can.
func Latest(ctx context.Context, opt Options) (Release, error) {
	base := opt.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	goos := opt.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := opt.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}

	rel, err := opt.getJSON(ctx, base+"/releases/latest")
	if err != nil {
		return Release{}, err
	}
	var meta struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(rel, &meta); err != nil {
		return Release{}, fmt.Errorf("selfupdate: release metadata is not the shape GitHub sends: %w", err)
	}
	if meta.TagName == "" {
		return Release{}, errors.New("selfupdate: release metadata carries no tag")
	}

	want := assetName(goos, goarch)
	var sumsURL string
	out := Release{Tag: meta.TagName, AssetName: want}
	for _, a := range meta.Assets {
		switch {
		case a.Name == want:
			out.AssetURL = a.BrowserDownloadURL
		case a.Name == "SHA256SUMS":
			sumsURL = a.BrowserDownloadURL
		}
	}
	if out.AssetURL == "" {
		return Release{}, fmt.Errorf("selfupdate: release %s ships no asset for %s/%s", meta.TagName, goos, goarch)
	}
	if sumsURL == "" {
		return Release{}, fmt.Errorf("selfupdate: release %s ships no SHA256SUMS; it cannot be verified", meta.TagName)
	}
	sums, err := opt.get(ctx, sumsURL)
	if err != nil {
		return Release{}, err
	}
	digest, ok := digestFor(string(sums), want)
	if !ok {
		return Release{}, fmt.Errorf("selfupdate: SHA256SUMS of release %s has no line for %s", meta.TagName, want)
	}
	out.SHA256 = digest
	return out, nil
}

// Compare compares two v-prefixed semantic versions, returning -1, 0 or 1. A
// current version that is not a release tag — a commit sha, "dev", a describe
// suffix — is an error: guessing whether a sha is older than v0.2.0 would make
// the command lie, and a command that replaces binaries must not lie.
func Compare(current, latest string) (int, error) {
	c, err := parseSemver(current)
	if err != nil {
		return 0, fmt.Errorf("selfupdate: current version %q is not a release tag: %w", current, err)
	}
	l, err := parseSemver(latest)
	if err != nil {
		return 0, fmt.Errorf("selfupdate: latest release %q is not a release tag: %w", latest, err)
	}
	for i := 0; i < 3; i++ {
		if c.num[i] != l.num[i] {
			if c.num[i] < l.num[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	// Equal numbers: a prerelease (v0.1.0-rc1) is OLDER than the release
	// (v0.1.0) — the semver rule, and the one that matters here, because a
	// release candidate must not read as up to date. Two prereleases compare
	// lexically, which is an approximation of the spec's identifier rules but
	// only ever decides between two pre-releases of the same triple.
	switch {
	case c.pre == l.pre:
		return 0, nil
	case c.pre != "" && l.pre == "":
		return -1, nil
	case c.pre == "" && l.pre != "":
		return 1, nil
	default:
		if c.pre < l.pre {
			return -1, nil
		}
		return 1, nil
	}
}

// semver is a parsed version: the three numbers plus any prerelease suffix.
type semver struct {
	num [3]int
	pre string
}

// parseSemver reads "vMAJOR.MINOR.PATCH", tolerating a prerelease or build
// suffix after the patch number. Anything else — a sha, "dev", an empty
// string — is rejected.
func parseSemver(v string) (semver, error) {
	var out semver
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 { // build metadata never affects precedence
		v = v[:i]
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		out.pre = v[i+1:]
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, errors.New("want vMAJOR.MINOR.PATCH")
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, fmt.Errorf("%q is not a number", p)
		}
		out.num[i] = n
	}
	return out, nil
}

// Apply downloads rel, verifies its digest, and renames it over exePath. The
// download streams straight into a temp file in exePath's directory — the same
// filesystem, so the final rename is atomic — and the digest is computed while
// copying. exePath's permissions are preserved; a binary that was 0755 stays
// 0755.
func Apply(ctx context.Context, opt Options, exePath string, rel Release) error {
	if err := opt.checkHTTPS(rel.AssetURL); err != nil {
		return err
	}
	dir := filepath.Dir(exePath)
	tmp, err := os.CreateTemp(dir, ".mabo-ctl-upgrade-")
	if err != nil {
		return fmt.Errorf("selfupdate: create temp file next to %s: %w", exePath, err)
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename

	info, err := os.Stat(exePath)
	if err != nil {
		return fmt.Errorf("selfupdate: stat %s: %w", exePath, err)
	}

	resp, err := opt.getResponse(ctx, rel.AssetURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("selfupdate: downloading %s: unexpected status %s", rel.AssetURL, resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return fmt.Errorf("selfupdate: downloading %s: %w", rel.AssetURL, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != rel.SHA256 {
		return fmt.Errorf("selfupdate: downloaded %s has sha256 %s, want %s; the file was not installed",
			rel.AssetName, got, rel.SHA256)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfupdate: write %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), info.Mode()); err != nil {
		return fmt.Errorf("selfupdate: set mode on %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), exePath); err != nil {
		return fmt.Errorf("selfupdate: replace %s: %w", exePath, err)
	}
	return nil
}

// checkHTTPS refuses any URL that is not https. The upgrade channel must never
// be talked into plain HTTP by a crafted release asset.
func (o Options) checkHTTPS(raw string) error {
	if o.AllowPlainHTTP {
		return nil
	}
	if u, err := url.Parse(raw); err != nil || u.Scheme != "https" {
		return fmt.Errorf("selfupdate: refusing non-https URL %q", raw)
	}
	return nil
}

// getJSON GETs url and returns the body. Every URL is scheme-checked here, so
// a caller cannot bypass the check by passing a crafted BaseURL.
func (o Options) getJSON(ctx context.Context, raw string) ([]byte, error) {
	if err := o.checkHTTPS(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("selfupdate: no release of mabo-ctl has been published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: fetching the latest release: unexpected status %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// get GETs url and returns the body, with the same guards as getJSON.
func (o Options) get(ctx context.Context, raw string) ([]byte, error) {
	resp, err := o.getResponse(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: downloading %s: unexpected status %s", raw, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// getResponse GETs url without reading the body; the caller closes it.
func (o Options) getResponse(ctx context.Context, raw string) (*http.Response, error) {
	if err := o.checkHTTPS(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: %w", err)
	}
	return resp, nil
}

// client returns opt's client or the default one with a timeout.
func (o Options) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// digestFor finds the digest of name in the contents of a SHA256SUMS file.
func digestFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], true
		}
	}
	return "", false
}
