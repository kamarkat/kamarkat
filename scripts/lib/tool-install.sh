#!/usr/bin/env bash
# Installing a required tool for the machine this is running on.
#
# Sourced, never executed. The caller provides `log`; everything else is here.
#
# ONE IMPLEMENTATION. There were three -- hub-bootstrap.sh, pre-req-cloud.sh and
# k8-setup/install-k8-tools.sh -- and all three had the same defect, because all
# three were written on a Windows workstation and copied. The one in the
# bootstrap path is the one that reached a tenant's CI and failed there; the
# others were never going to be found by using them, since a workstation that
# already has the tools never runs an installer.
#
# See the function comments for what the defect was and why each rule here
# exists. Companion to lib/tool-path.sh, which makes an INSTALLED tool findable;
# this one puts it there.

# The caller owns reporting: this library is sourced into a script that already
# logs with a timestamp and a destination, and a second convention would make one
# run speak in two voices. Required rather than defaulted, because a silent
# installer is how "✓ installed" and "could not be installed" ended up in the
# same run.
if ! declare -F log >/dev/null 2>&1; then
    echo "lib/tool-install.sh: the sourcing script must define log()" >&2
    return 1
fi

# Install a required tool for THIS invocation, for the OS and CPU it is running on.
#
# Every branch here downloaded a Windows executable -- kind-windows-amd64,
# clusterctl-windows-amd64.exe, a helm .zip, jq.exe -- whatever the host was.
# On a Linux runner that produced "✓ clusterctl installed to ~/bin/clusterctl.exe"
# followed immediately by "could not be auto-installed": it had downloaded a PE
# binary, chmod'ed it, and found it unrunnable. Written for a Windows
# workstation, and unreachable from CI until the tenant launcher started getting
# this far.
#
# PINNED, never /latest/: a bootstrap that resolves a different tool version on
# each run is not the same bootstrap. VERIFIED, because an unauthenticated
# download that becomes a cluster's control plane deserves a checksum. And
# installed into a directory owned by this run, added to this PATH only -- no
# sudo, nothing global, and nothing appended to a shell profile, which outlives
# the run that wrote it.
TOOLS_DIR="${TOOLS_DIR:-}"

# tool_platform -> "<os>-<arch>" in the spelling upstream uses, or a refusal.
tool_platform() {
    local os arch
    case "$(uname -s)" in
        Linux)                      os="linux" ;;
        Darwin)                     os="darwin" ;;
        MINGW*|MSYS*|CYGWIN*)       os="windows" ;;
        *) log "ERROR: $(uname -s) is not a platform this bootstrap installs tools for"
           return 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64)               arch="amd64" ;;
        arm64|aarch64)              arch="arm64" ;;
        *) log "ERROR: $(uname -m) is not an architecture this bootstrap installs tools for"
           return 1 ;;
    esac
    printf '%s-%s' "$os" "$arch"
}

# sha256_of <file> -- GNU coreutils on Linux, BSD shasum on macOS.
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# fetch_tool <url> <dest> [checksum_url]
#
# Verified against the checksum upstream publishes for that exact artefact.
# kind, helm and jq each publish one; the Cluster API project publishes none for
# clusterctl, so for that one the version pin and the release URL over TLS are
# the whole of what is trusted, and the run says so rather than leaving silence
# to be read as verification.
#
# A checksum served from the same release proves the transfer, not the
# provenance. It catches a truncated download, a proxy's error page saved as a
# binary, and a mutated asset -- which is what was actually failing here.
fetch_tool() {
    local url="$1" dest="$2" sum_url="${3:-}" name
    name="$(basename "$url")"
    curl -fsSL --retry 3 --retry-connrefused -o "$dest" "$url" || {
        log "ERROR: could not download $url"
        return 1
    }
    if [[ -z "$sum_url" ]]; then
        log "  (upstream publishes no checksum for $name; pinned by version and URL)"
        chmod +x "$dest"
        return 0
    fi

    local sums="${dest}.sums" want got
    curl -fsSL --retry 3 -o "$sums" "$sum_url" || {
        log "ERROR: could not download the checksum for $name from $sum_url"
        rm -f "$dest"
        return 1
    }
    # "<digest>  <name>", or "<digest> *<name>" in binary mode. The artefact is
    # named explicitly: a multi-entry file lists every platform, and verifying
    # against whichever line came first would pass a mismatched binary.
    want="$(awk -v n="$name" '{sub(/^\*/, "", $2); if ($2 == n) {print $1; exit}}' "$sums")"
    rm -f "$sums"
    if [[ -z "$want" ]]; then
        log "ERROR: $sum_url does not list $name"
        rm -f "$dest"
        return 1
    fi
    got="$(sha256_of "$dest")"
    if [[ "$got" != "$want" ]]; then
        log "ERROR: $name does not match its published checksum"
        log "       expected $want"
        log "       got      $got"
        rm -f "$dest"
        return 1
    fi
    log "  ✓ $name checksum verified"
    chmod +x "$dest"
}

auto_install_tool() {
    local tool="$1" plat
    plat="$(tool_platform)" || return 1

    # Owned by this invocation. The previous installer wrote to ~/bin and
    # appended to ~/.bashrc, so the tools a run installed were found only by a
    # later interactive shell -- not by the run that installed them.
    if [[ -z "$TOOLS_DIR" ]]; then
        TOOLS_DIR="$(mktemp -d)"
        export PATH="$TOOLS_DIR:$PATH"
        log "  tools for this run: $TOOLS_DIR"
    fi
    local ext=""
    [[ "$plat" == windows-* ]] && ext=".exe"

    case "$tool" in
        kind)
            local v="v0.27.0"
            log "  Auto-installing kind $v for $plat..."
            local kind_url="https://github.com/kubernetes-sigs/kind/releases/download/${v}/kind-${plat}${ext}"
            fetch_tool "$kind_url" "$TOOLS_DIR/kind${ext}" "${kind_url}.sha256sum" || return 1
            ;;
        clusterctl)
            # The version this release is tested with. Unpinning it means a
            # bootstrap whose Cluster API client changes under it.
            local v="v1.13.7"
            log "  Auto-installing clusterctl $v for $plat..."
            fetch_tool "https://github.com/kubernetes-sigs/cluster-api/releases/download/${v}/clusterctl-${plat}${ext}" \
                "$TOOLS_DIR/clusterctl${ext}" || return 1
            ;;
        helm)
            local v="v3.17.3" tgz="$TOOLS_DIR/helm.tgz"
            log "  Auto-installing helm $v for $plat..."
            if [[ "$plat" == windows-* ]]; then
                log "ERROR: install helm yourself on Windows; its archive is a zip and"
                log "       this bootstrap does not carry an unzip dependency"
                return 1
            fi
            local helm_url="https://get.helm.sh/helm-${v}-${plat}.tar.gz"
            fetch_tool "$helm_url" "$tgz" "${helm_url}.sha256sum" || return 1
            tar -xzf "$tgz" -C "$TOOLS_DIR" --strip-components=1 "${plat}/helm" || {
                log "ERROR: could not unpack helm from $tgz"
                return 1
            }
            rm -f "$tgz"
            chmod +x "$TOOLS_DIR/helm"
            ;;
        jq)
            local v="1.7.1" name
            case "$plat" in
                linux-amd64)  name="jq-linux-amd64" ;;
                linux-arm64)  name="jq-linux-arm64" ;;
                darwin-amd64) name="jq-macos-amd64" ;;
                darwin-arm64) name="jq-macos-arm64" ;;
                windows-*)    name="jq-windows-amd64.exe" ;;
            esac
            log "  Auto-installing jq $v for $plat..."
            local jq_base="https://github.com/jqlang/jq/releases/download/jq-${v}"
            fetch_tool "${jq_base}/${name}" "$TOOLS_DIR/jq${ext}" "${jq_base}/sha256sum.txt" || return 1
            ;;
        *)
            log "ERROR: no installer for '$tool'; install it and re-run"
            return 1
            ;;
    esac
    log "  ✓ $tool installed to $TOOLS_DIR"
}
