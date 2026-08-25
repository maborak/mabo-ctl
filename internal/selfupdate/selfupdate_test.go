package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveLatest starts an httptest server standing in for GitHub: it answers
// /releases/latest with a v1.2.3 release whose darwin-arm64 asset holds body,
// and whose SHA256SUMS carries that body's real digest. mutate rewrites the
// JSON on its way out, which is how a test breaks one thing at a time.
func serveLatest(t *testing.T, body []byte, mutate func(js string) string) Options {
	t.Helper()
	var srvURL string
	mux := http.NewServeMux()
	asset := func(name string, content []byte) string {
		mux.HandleFunc("/"+name, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(content)
		})
		return srvURL + "/" + name
	}
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		sum := sha256.Sum256(body)
		js := fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[
			{"name":"mabo-ctl-darwin-arm64","browser_download_url":%q},
			{"name":"SHA256SUMS","browser_download_url":%q}]}`,
			asset("mabo-ctl-darwin-arm64", body),
			asset("SHA256SUMS", []byte(hex.EncodeToString(sum[:])+"  mabo-ctl-darwin-arm64\n")))
		if mutate != nil {
			js = mutate(js)
		}
		_, _ = w.Write([]byte(js))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	srvURL = srv.URL
	// httptest speaks plain HTTP, which is exactly what the production gate
	// forbids — the seam exists so the gate can stay on in production.
	return Options{BaseURL: srv.URL, GOOS: "darwin", GOARCH: "arm64", AllowPlainHTTP: true}
}

// TestLatestResolvesThePlatformAsset walks the happy path: the right asset is
// picked for the requested platform and its digest comes from SHA256SUMS.
func TestLatestResolvesThePlatformAsset(t *testing.T) {
	body := []byte("fake binary")
	opt := serveLatest(t, body, nil)

	rel, err := Latest(context.Background(), opt)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.Tag != "v1.2.3" {
		t.Errorf("Tag = %q, want v1.2.3", rel.Tag)
	}
	if rel.AssetName != "mabo-ctl-darwin-arm64" {
		t.Errorf("AssetName = %q, want the darwin-arm64 asset", rel.AssetName)
	}
	sum := sha256.Sum256(body)
	if rel.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %q, want the digest of the served body", rel.SHA256)
	}
}

// TestLatestRejectsAnUnverifiableRelease: no SHA256SUMS, no upgrade — the
// command must not fall back to trusting the download.
func TestLatestRejectsAnUnverifiableRelease(t *testing.T) {
	opt := serveLatest(t, []byte("x"), func(js string) string {
		// Rename the checksum asset out from under the release: the URL stays,
		// the name no longer matches what Latest looks for.
		return strings.Replace(js, `{"name":"SHA256SUMS"`, `{"name":"NOTES"`, 1)
	})
	if _, err := Latest(context.Background(), opt); err == nil {
		t.Fatal("Latest accepted a release with no SHA256SUMS")
	}
}

// TestLatestRejectsAMissingPlatformAsset: a release that does not ship this
// platform is an error, not an empty release.
func TestLatestRejectsAMissingPlatformAsset(t *testing.T) {
	opt := serveLatest(t, []byte("x"), func(js string) string {
		return strings.Replace(js, "mabo-ctl-darwin-arm64", "mabo-ctl-solaris-sparc", 1)
	})
	if _, err := Latest(context.Background(), opt); err == nil {
		t.Fatal("Latest accepted a release without this platform's asset")
	}
}

// TestLatestSaysSoWhenNoReleaseExists: a 404 is the honest "nothing published
// yet", not a generic failure.
func TestLatestSaysSoWhenNoReleaseExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	_, err := Latest(context.Background(), Options{BaseURL: srv.URL, AllowPlainHTTP: true})
	if err == nil || !strings.Contains(err.Error(), "no release") {
		t.Fatalf("err = %v, want the no-release-yet message", err)
	}
}

// TestLatestRefusesPlainHTTP pins the channel rule: the metadata URL itself is
// scheme-checked, so a caller cannot downgrade the channel via BaseURL.
func TestLatestRefusesPlainHTTP(t *testing.T) {
	_, err := Latest(context.Background(), Options{BaseURL: "http://api.github.com/repos/maborak/mabo-ctl"})
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want a refusal of the non-https URL", err)
	}
}

// TestCompare covers the tag arithmetic the upgrade decision rests on.
func TestCompare(t *testing.T) {
	cases := []struct {
		current, latest string
		want            int
		wantErr         bool
	}{
		{"v0.1.0", "v0.1.0", 0, false},
		{"v0.1.0", "v0.2.0", -1, false},
		{"v1.2.3", "v1.2.3", 0, false},
		{"v1.2.3", "v1.2.4", -1, false},
		{"v2.0.0", "v1.9.9", 1, false},
		{"v0.1.0-rc1", "v0.1.0", -1, false},
		{"dev", "v0.1.0", 0, true},
		{"baac8ee", "v0.1.0", 0, true},
		{"", "v0.1.0", 0, true},
		{"v0.1", "v0.1.0", 0, true},
		{"vX.1.0", "v0.1.0", 0, true},
	}
	for _, tc := range cases {
		got, err := Compare(tc.current, tc.latest)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Compare(%q, %q) = %d, want an error", tc.current, tc.latest, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Compare(%q, %q): %v", tc.current, tc.latest, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", tc.current, tc.latest, got, tc.want)
		}
	}
}

// TestApplySwapsTheBinaryOnAMatchingDigest runs the whole swap against a real
// file: same digest installs, wrong digest leaves the original untouched and
// no temp file behind.
func TestApplySwapsTheBinaryOnAMatchingDigest(t *testing.T) {
	old := []byte("#!/bin/sh\nexit 0\n")
	new := []byte("#!/bin/sh\necho upgraded\n")
	sum := sha256.Sum256(new)
	digest := hex.EncodeToString(sum[:])
	rel := Release{
		Tag:       "v9.9.9",
		AssetName: "mabo-ctl-darwin-arm64",
		SHA256:    digest,
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "mabo-ctl")
	if err := os.WriteFile(exe, old, 0o755); err != nil {
		t.Fatalf("write exe: %v", err)
	}

	t.Run("wrong digest aborts", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("something else entirely"))
		}))
		t.Cleanup(srv.Close)
		bad := rel
		bad.AssetURL = srv.URL
		if err := Apply(context.Background(), Options{Client: srv.Client(), AllowPlainHTTP: true}, exe, bad); err == nil {
			t.Fatal("Apply accepted a download with the wrong digest")
		}
		if got, err := os.ReadFile(exe); err != nil || string(got) != string(old) {
			t.Fatalf("the failed upgrade damaged the installed binary: %q, %v", got, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("the failed upgrade left files behind: %d entries", len(entries))
		}
	})

	t.Run("matching digest installs", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(new)
		}))
		t.Cleanup(srv.Close)
		good := rel
		good.AssetURL = srv.URL
		if err := Apply(context.Background(), Options{Client: srv.Client(), AllowPlainHTTP: true}, exe, good); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		got, err := os.ReadFile(exe)
		if err != nil || string(got) != string(new) {
			t.Fatalf("the binary was not replaced: %q, %v", got, err)
		}
		info, err := os.Stat(exe)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Errorf("mode = %v, want the original 0755", info.Mode().Perm())
		}
	})
}

// TestApplyRefusesPlainHTTP: the asset URL is checked before anything is
// fetched from it.
func TestApplyRefusesPlainHTTP(t *testing.T) {
	rel := Release{AssetURL: "http://example.invalid/mabo-ctl", SHA256: "x"}
	if err := Apply(context.Background(), Options{}, filepath.Join(t.TempDir(), "mabo-ctl"), rel); err == nil {
		t.Fatal("Apply accepted a non-https asset URL")
	}
}
