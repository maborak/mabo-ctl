#!/bin/sh
# mabo-ctl installer — downloads the latest release binary, verifies it against
# the release's SHA256SUMS, and installs it into a directory on your PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/maborak/mabo-ctl/main/install.sh | sh
#   DESTDIR=~/.local/bin curl -fsSL ... | sh   # install somewhere else
#
# Nothing runs from the network: the script itself is a shell script, and the
# binary it downloads is verified against the SHA256SUMS the release shipped
# before a single byte is written. It installs to /usr/local/bin when that is
# writable, or ~/.local/bin for a normal user (or $DESTDIR when provided), and
# relies on `mabo-ctl upgrade` for everything after that.
set -eu

repo="maborak/mabo-ctl"
dest="${DESTDIR:-}"
explicit_dest="${DESTDIR+x}"

# A GITHUB_TOKEN (or GH_TOKEN) in the environment authenticates an otherwise
# private repository: GitHub serves a private repo's release assets ONLY
# through the API, never through the public download URLs, so the script uses
# the API download endpoint when a token is present and the public URLs when
# it is not. For a public repository neither is required.
token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
api_auth() {
  if [ -n "$token" ]; then
    printf '%s' "-H Authorization: token $token"
  fi
}

say() { printf 'mabo-ctl: %s\n' "$*"; }

die() {
  say "error: $*" >&2
  exit 1
}

if [ -z "$dest" ]; then
  if [ "$(id -u)" -eq 0 ]; then
    dest=/usr/local/bin
  elif mkdir -p /usr/local/bin 2>/dev/null && [ -w /usr/local/bin ]; then
    dest=/usr/local/bin
  elif [ -n "${HOME:-}" ]; then
    dest="$HOME/.local/bin"
  else
    die "cannot find a writable install directory; set DESTDIR to one"
  fi
fi

# A caller may pass DESTDIR=~/bin through an environment file rather than as a
# shell assignment, so expand the common leading-tilde form without eval.
case "$dest" in
  '~/'*) [ -n "${HOME:-}" ] || die "DESTDIR starts with ~/ but HOME is not set"; dest="$HOME/${dest#~/}" ;;
esac

# Map the running machine to an asset name. The release ships one static
# CGO_ENABLED=0 binary per OS/arch; anything else is a refusal, not a guess.
# uname reports the arch differently across systems (aarch64 on Linux ARM,
# arm64 on macOS), so normalise before matching.
machine="$(uname -s)-$(uname -m)"
case "$machine" in
  Darwin-arm64)   asset="mabo-ctl-darwin-arm64" ;;
  Darwin-x86_64)  asset="mabo-ctl-darwin-amd64" ;;
  Darwin-i386)    asset="mabo-ctl-darwin-amd64" ;;
  Linux-aarch64)  asset="mabo-ctl-linux-arm64" ;;
  Linux-arm64)    asset="mabo-ctl-linux-arm64" ;;
  Linux-x86_64)   asset="mabo-ctl-linux-amd64" ;;
  Linux-i386)     asset="mabo-ctl-linux-amd64" ;;
  *) die "no prebuilt mabo-ctl for $(uname -s) $(uname -m); " \
        "install it with: go install ${repo}/cmd/mabo-ctl@latest" ;;
esac

if [ "$(id -u)" -eq 0 ]; then
  say "running as root; installing directly into $dest"
else
  # The umask matters: this binary can start and stop local processes. A world-
  # writable copy is a local privilege escalation waiting for an attacker who
  # can write anywhere in its directory, so make the file 0755 with a strict
  # process umask instead of relying on the caller's.
  umask 022
fi

mkdir -p "$dest" 2>/dev/null || true
if [ ! -w "$dest" ]; then
  if [ -z "$explicit_dest" ] && [ "$(id -u)" -ne 0 ] && [ -n "${HOME:-}" ]; then
    dest="$HOME/.local/bin"
    mkdir -p "$dest" 2>/dev/null || true
  fi
  [ -w "$dest" ] || die "$dest is not writable; re-run as root, or set DESTDIR to a writable directory"
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/mabo-ctl.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT INT TERM

say "resolving latest release…"
if [ -n "$token" ]; then
  # Private-repo path: the API names the latest tag and the asset IDs, and
  # serves the files through its own endpoint with the token and an
  # octet-stream accept. awk walks the JSON to pair each "name" with the "id"
  # that follows it.
  api="https://api.github.com/repos/${repo}/releases/latest"
  latest_json="$(curl -fsSL -H "Authorization: token $token" "$api")"
  tag="$(printf '%s' "$latest_json" | sed -n 's/^  "tag_name": "\([^"]*\)",/\1/p' | head -n1)"
  [ -n "$tag" ] || die "could not read the latest release tag from the API"
  asset_id="$(printf '%s' "$latest_json" | awk -v n="$asset" '
    /"id":/ {id=$2}
    /"name":/ && $2 ~ n {gsub(/[",]/,"",id); print id; exit}
  ')"
  [ -n "$asset_id" ] || die "the latest release does not ship $asset"
  sum_id="$(printf '%s' "$latest_json" | awk -v n="SHA256SUMS" '
    /"id":/ {id=$2}
    /"name":/ && $2 ~ n {gsub(/[",]/,"",id); print id; exit}
  ')"
  [ -n "$sum_id" ] || die "the latest release does not ship SHA256SUMS"
  download="https://api.github.com/repos/${repo}/releases/assets/${asset_id}"
  sum_download="https://api.github.com/repos/${repo}/releases/assets/${sum_id}"
  dl() { curl -fsSL -H "Authorization: token $token" -H "Accept: application/octet-stream" "$1" -o "$2"; }
else
  # Public-repo path: no auth, the download URLs are enough, and the tag comes
  # from the redirect /releases/latest follows.
  tag="$(curl -fsSLI "https://github.com/${repo}/releases/latest" \
    | sed -n 's/^[Ll]ocation: .*\/tag\///p' | tr -d '\r' | tail -n1)"
  [ -n "$tag" ] || die "could not read the latest release tag from the redirect"
  download="https://github.com/${repo}/releases/latest/download/${asset}"
  sum_download="https://github.com/${repo}/releases/latest/download/SHA256SUMS"
  dl() { curl -fsSL "$1" -o "$2"; }
fi
say "installing ${tag} (${asset})"

dl "$download" "$tmp/$asset"
dl "$sum_download" "$tmp/SHA256SUMS"

# Verify before installing. macOS ships shasum, Linux sha256sum; fall back to
# a portable check when neither exists.
if command -v shasum >/dev/null 2>&1; then
  sum="$(cd "$tmp" && shasum -a 256 "$asset" | awk '{print $1}')"
elif command -v sha256sum >/dev/null 2>&1; then
  sum="$(cd "$tmp" && sha256sum "$asset" | awk '{print $1}')"
else
  die "neither shasum nor sha256sum is available; refusing to install unverified"
fi
want="$(awk -v a="$asset" '$2 == a {print $1}' "$tmp/SHA256SUMS" | head -n1)"
[ -n "$want" ] || die "SHA256SUMS does not list $asset"
[ "$sum" = "$want" ] || die "checksum mismatch (got $sum, want $want); the download is not the release"

install -m 0755 "$tmp/$asset" "$dest/mabo-ctl"

say "installed $dest/mabo-ctl ($tag)"
"$dest/mabo-ctl" --version | head -n1 | sed 's/^/  /'
case ":$PATH:" in
  *":$dest:"*) ;;
  *)
    rc=""
    case "${SHELL##*/}" in
      zsh) rc="${ZDOTDIR:-${HOME:-}}/.zshrc" ;;
      bash) rc="${HOME:-}/.bashrc" ;;
    esac
    if [ -z "$rc" ] && [ -n "${HOME:-}" ]; then
      if [ -f "${HOME}/.zshrc" ]; then
        rc="${HOME}/.zshrc"
      elif [ -f "${HOME}/.bashrc" ]; then
        rc="${HOME}/.bashrc"
      fi
    fi
    if [ -n "$rc" ] && [ -n "${HOME:-}" ]; then
      mkdir -p "${rc%/*}" 2>/dev/null || true
      touch "$rc" 2>/dev/null || true
      if ! grep -Fqx "export PATH=\"\$PATH:$dest\"" "$rc" 2>/dev/null; then
        printf '\n# mabo-ctl\nexport PATH="\$PATH:%s"\n' "$dest" >> "$rc" 2>/dev/null || true
      fi
      if grep -Fqx "export PATH=\"\$PATH:$dest\"" "$rc" 2>/dev/null; then
        say "added $dest to PATH in $rc (open a new shell)"
      else
        say "note: $dest is not on your PATH; add it to $rc"
      fi
    else
      say "note: $dest is not on your PATH; add it to your shell startup file"
    fi
    ;;
esac
say "next: drop a mabo-ctl.yaml at your repo root and run mabo-ctl"
