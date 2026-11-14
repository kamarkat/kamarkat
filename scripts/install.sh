#!/bin/sh
# Install the kakt CLI.
#
# Served as the one-command install. It is a THIN convenience over the GitHub
# release, which stays the only place a binary comes from: this detects the
# platform, resolves a version, verifies the download against the release's own
# checksum file, and puts the binary somewhere on PATH. It does not update
# itself, carry a package index, or know anything about a box.
#
# INSTALLING THE CLI IS NOT UPGRADING A PLATFORM. A box runs the bundle version
# its repository declares and nothing here changes that. A newer CLI on a
# workstation leaves every running box where it is until its owner adopts a
# change.
#
# POSIX sh, because it is piped to whatever /bin/sh is on the machine.
set -eu

REPO="kamarkat/kamarkat"
VERSION=""
BIN_DIR=""

usage() {
    cat >&2 <<USAGE
Install the kakt CLI.

  --version <X.Y.Z>   install this version (default: the newest release)
  --bin-dir <path>    install here (default: the first writable of
                      \$HOME/.local/bin, \$HOME/bin, /usr/local/bin)
  --help              this text

The binary is downloaded from https://github.com/${REPO}/releases and verified
against that release's SHA256SUMS before it is installed.
USAGE
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
        --version=*) VERSION="${1#*=}"; shift ;;
        --bin-dir) BIN_DIR="${2:?--bin-dir needs a value}"; shift 2 ;;
        --bin-dir=*) BIN_DIR="${1#*=}"; shift ;;
        --help|-h) usage; exit 0 ;;
        *) echo "install: unknown argument '$1'" >&2; usage; exit 2 ;;
    esac
done

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "install: $1 is required and was not found" >&2
        exit 1
    }
}
need curl

# PLATFORM, NOT A GUESS. An unsupported pair is refused by name: a wrong binary
# fails later with "cannot execute binary file", which says nothing about why.
os=$(uname -s)
arch=$(uname -m)
case "$os" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) echo "install: $os is not supported. The release publishes linux and darwin binaries." >&2; exit 1 ;;
esac
case "$arch" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "install: $arch is not supported. The release publishes amd64 and arm64 binaries." >&2; exit 1 ;;
esac
asset="kakt-${os}-${arch}"

# The newest release, asked of GitHub rather than assembled from a guess about
# tag naming.
if [ -z "$VERSION" ]; then
    VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | sed -n 's/.*"tag_name": *"v\{0,1\}\([^"]*\)".*/\1/p' | head -1)
    [ -n "$VERSION" ] || {
        echo "install: could not read the newest release from GitHub." >&2
        echo "  Name one with --version, or see https://github.com/${REPO}/releases" >&2
        exit 1
    }
fi
VERSION="${VERSION#v}"
base="https://github.com/${REPO}/releases/download/v${VERSION}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "kakt ${VERSION} (${os}/${arch})"

curl -fsSL -o "$tmp/$asset" "${base}/${asset}" 2>/dev/null || {
    echo "install: ${asset} is not published for v${VERSION}." >&2
    echo "  See https://github.com/${REPO}/releases/tag/v${VERSION}" >&2
    exit 1
}

# VERIFIED, OR NOT INSTALLED. The checksum file is part of the release, so a
# tampered binary and a truncated download are both caught -- and a release that
# publishes no checksums is refused rather than trusted, because an unverifiable
# download is the thing this exists to prevent.
curl -fsSL -o "$tmp/SHA256SUMS" "${base}/SHA256SUMS" 2>/dev/null || {
    echo "install: v${VERSION} publishes no SHA256SUMS, so this download cannot be" >&2
    echo "  verified. Refusing to install it. Releases from 0.2.6 carry one." >&2
    exit 1
}
want=$(grep " ${asset}\$" "$tmp/SHA256SUMS" | awk '{print $1}')
[ -n "$want" ] || { echo "install: SHA256SUMS does not list ${asset}" >&2; exit 1; }
if command -v shasum >/dev/null 2>&1; then
    got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
elif command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
    echo "install: neither shasum nor sha256sum is available, so the download" >&2
    echo "  cannot be verified. Refusing to install it." >&2
    exit 1
fi
[ "$got" = "$want" ] || {
    echo "install: ${asset} does not match the checksum this release publishes." >&2
    echo "  expected $want" >&2
    echo "  got      $got" >&2
    echo "  Nothing was installed." >&2
    exit 1
}
echo "verified against the release's SHA256SUMS"

# A directory this user can write, so installing needs no sudo. /usr/local/bin
# last, and only when it is already writable.
if [ -z "$BIN_DIR" ]; then
    for candidate in "$HOME/.local/bin" "$HOME/bin" /usr/local/bin; do
        if [ -d "$candidate" ] && [ -w "$candidate" ]; then BIN_DIR="$candidate"; break; fi
    done
fi
if [ -z "$BIN_DIR" ]; then
    BIN_DIR="$HOME/.local/bin"
    mkdir -p "$BIN_DIR"
fi
[ -d "$BIN_DIR" ] || mkdir -p "$BIN_DIR"
[ -w "$BIN_DIR" ] || { echo "install: $BIN_DIR is not writable. Pass --bin-dir." >&2; exit 1; }

# Replaced by rename, which is atomic: a half-written binary on PATH is worse
# than an old one, and re-running must not corrupt what is already installed.
chmod 0755 "$tmp/$asset"
mv -f "$tmp/$asset" "$BIN_DIR/kakt"
echo "installed $BIN_DIR/kakt"

case ":${PATH}:" in
    *":${BIN_DIR}:"*) ;;
    *) echo ""
       echo "$BIN_DIR is not on your PATH. Add it:"
       echo "  export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

echo ""
echo "$("$BIN_DIR/kakt" bundle-version 2>/dev/null || echo "$VERSION") is the platform version this CLI carries."
echo "Next: kakt box init"
