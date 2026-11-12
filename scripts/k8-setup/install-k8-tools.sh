#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Day-0's working copy, in the operator's runtime path (internal/kakt-cli/opstore).
HUB_KUBECONFIG="${KAMARKAT_HOME:-$HOME/.kamarkat}/run/kubeconfig/hub.kubeconfig"

# Ensure ~/bin exists and is in PATH
mkdir -p ~/bin
case ":$PATH:" in
  *":$HOME/bin:"*) ;;
  *) export PATH="$HOME/bin:$PATH" ;;
esac

# Detect OS/ARCH for binary downloads
detect_platform() {
  local raw_os
  raw_os="$(uname -s)"
  case "${raw_os}" in
    MINGW*|MSYS*|CYGWIN*) OS="windows"; EXT=".exe" ;;
    Darwin) OS="darwin"; EXT="" ;;
    Linux)  OS="linux"; EXT="" ;;
    *)      OS="$(echo "${raw_os}" | tr '[:upper:]' '[:lower:]')"; EXT="" ;;
  esac

  local raw_arch
  raw_arch="$(uname -m)"
  case "${raw_arch}" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) ARCH="${raw_arch}" ;;
  esac
}

detect_platform

install_go() {
  if command -v go &>/dev/null; then
    echo "Go already installed: $(go version 2>/dev/null || true)"
    return
  fi

  # Read required Go version from go.mod
  local required_version
  required_version="$(grep '^go ' "${SCRIPT_DIR}/../../go.mod" | awk '{print $2}')"
  if [ -z "${required_version}" ]; then
    required_version="1.26.0"
  fi
  echo "Installing Go ${required_version}..."

  local go_tarball="go${required_version}.${OS}-${ARCH}.tar.gz"
  local go_url="https://go.dev/dl/${go_tarball}"

  if [ "${OS}" = "windows" ]; then
    go_tarball="go${required_version}.${OS}-${ARCH}.zip"
    go_url="https://go.dev/dl/${go_tarball}"
    curl -sSL -o "/tmp/${go_tarball}" "${go_url}"
    rm -rf "${HOME}/go"
    unzip -qo "/tmp/${go_tarball}" -d "${HOME}" 2>/dev/null || {
      # If unzip not available, try PowerShell
      powershell -Command "Expand-Archive -Path '/tmp/${go_tarball}' -DestinationPath '${HOME}' -Force"
    }
  else
    curl -sSL -o "/tmp/${go_tarball}" "${go_url}"
    # Extract to a temp dir then move — avoids nesting if ~/go already exists
    rm -rf "/tmp/go-extract"
    mkdir -p "/tmp/go-extract"
    tar -C "/tmp/go-extract" -xzf "/tmp/${go_tarball}"
    rm -rf "${HOME}/go"
    mv "/tmp/go-extract/go" "${HOME}/go"
    rm -rf "/tmp/go-extract"
  fi
  rm -f "/tmp/${go_tarball}"

  # Add go/bin to PATH if not already there
  case ":$PATH:" in
    *":${HOME}/go/bin:"*) ;;
    *) export PATH="${HOME}/go/bin:${PATH}" ;;
  esac

  # Also append to ~/.bashrc / ~/.zshrc for future shells
  local rc_file="${HOME}/.bashrc"
  if [ -f "${HOME}/.zshrc" ]; then
    rc_file="${HOME}/.zshrc"
  fi
  if ! grep -q 'export PATH="${HOME}/go/bin:${PATH}"' "${rc_file}" 2>/dev/null; then
    echo "" >> "${rc_file}"
    echo '# Go' >> "${rc_file}"
    echo 'export PATH="${HOME}/go/bin:${PATH}"' >> "${rc_file}"
  fi

  echo "Go ${required_version} installed to ${HOME}/go"
}

install_kubectl() {
  if command -v kubectl &>/dev/null; then
    echo "kubectl already installed: $(kubectl version --client --short 2>/dev/null || true)"
    return
  fi
  echo "Installing kubectl..."
  local ver
  ver="$(curl -L -s https://dl.k8s.io/release/stable.txt)"
  curl -Lo ~/bin/kubectl${EXT:-} "https://dl.k8s.io/release/${ver}/bin/${OS}/${ARCH}/kubectl${EXT:-}"
  chmod +x ~/bin/kubectl${EXT:-}
  echo "kubectl installed to ~/bin/kubectl${EXT:-}"
}

install_argocd() {
  if command -v argocd &>/dev/null && argocd version --client --short &>/dev/null; then
    echo "argocd CLI already installed: $(argocd version --client --short 2>/dev/null || true)"
    return
  fi
  echo "Installing argocd CLI..."
  rm -f ~/bin/argocd*
  local url="https://github.com/argoproj/argo-cd/releases/latest/download/argocd-${OS}-${ARCH}${EXT:-}"
  echo "Downloading: ${url}"
  curl -sSL -o ~/bin/argocd${EXT:-} "${url}"
  chmod +x ~/bin/argocd${EXT:-}
  echo "argocd CLI installed to ~/bin/argocd${EXT:-}"
}

install_gh() {
  if command -v gh &>/dev/null; then
    echo "gh already installed: $(gh version 2>/dev/null | head -1 || true)"
    return
  fi
  echo "Installing gh CLI..."
  rm -f ~/bin/gh*

  if [ "${OS}" = "windows" ]; then
    local gh_zip="${HOME}/bin/gh.zip"
    curl -sSL -o "${gh_zip}" "https://github.com/cli/cli/releases/latest/download/gh_2.70.0_windows_amd64.zip"
    unzip -jo "${gh_zip}" "bin/gh.exe" -d "${HOME}/bin" 2>/dev/null
    rm -f "${gh_zip}"
  else
    local gh_tar="${HOME}/bin/gh.tar.gz"
    curl -sSL -o "${gh_tar}" "https://github.com/cli/cli/releases/latest/download/gh_2.70.0_${OS}_${ARCH}.tar.gz"
    tar -xzf "${gh_tar}" -C /tmp
    mv /tmp/gh_*/bin/gh "${HOME}/bin/gh${EXT:-}"
    rm -rf /tmp/gh_*
    rm -f "${gh_tar}"
  fi
  chmod +x ~/bin/gh${EXT:-}
  echo "gh CLI installed to ~/bin/gh${EXT:-}"
}

sync_via_argo() {
  local app="$1"

  echo "Starting port-forward to argocd-server..."
  KUBECONFIG="${HUB_KUBECONFIG}" kubectl port-forward -n platform-ops svc/argocd-server 8080:80 &>/tmp/argocd-pf.log &
  local PF_PID=$!
  sleep 3

  local ADMIN_PASSWORD
  ADMIN_PASSWORD=$(KUBECONFIG="${HUB_KUBECONFIG}" kubectl get secret argocd-secret -n platform-ops -o jsonpath='{.data.admin\.password}' | base64 -d 2>/dev/null || true)

  argocd login localhost:8080 --username admin --password "${ADMIN_PASSWORD}" --insecure 2>&1 || true
  argocd app sync "${app}" --grpc-web 2>&1 || true
  argocd app wait "${app}" --health --grpc-web 2>&1 || true

  kill "${PF_PID}" 2>/dev/null || true
}

sync_apps() {
  local app="${1:-}"
  if [ -z "${app}" ]; then
    echo "Usage: $0 <app-name>"
    echo "Example: $0 tenant-waypoint-workloads"
    exit 1
  fi

  # Try argocd CLI first; fall back to kubectl patch if it hangs
  if command -v argocd &>/dev/null && argocd version --client --short &>/dev/null; then
    sync_via_argo "${app}"
  else
    echo "argocd CLI not available or slow on this platform."
    echo "Syncing via kubectl patch instead..."
    KUBECONFIG="${HUB_KUBECONFIG}" kubectl patch application "${app}" -n platform-ops \
      --type merge -p '{"metadata":{"annotations":{"argocd.argoproj.io/refresh":"hard"}}}' 2>&1
    echo "Hard refresh triggered for ${app}"
    echo "ArgoCD will re-attempt sync automatically."
  fi
}

install_go
install_kubectl
install_argocd
install_gh

# Persist GitHub token to ~/.bashrc so gh works in all shells
# The operator's own store, not a checkout (internal/kakt-cli/opstore).
GH_TOKEN_FILE="${KAMARKAT_HOME:-$HOME/.kamarkat}/github/github-pat-token"
if [[ ! -f "$GH_TOKEN_FILE" ]]; then
  echo "ERROR: GitHub PAT not found at $GH_TOKEN_FILE"
  echo "  Create the file with your GitHub Personal Access Token:"
  echo "  echo 'ghp_your_token_here' > $GH_TOKEN_FILE"
  exit 1
fi

gh_token_value="$(cat "$GH_TOKEN_FILE")"
export GH_TOKEN="$gh_token_value"
rc_file="${HOME}/.bashrc"
if [ -f "${HOME}/.zshrc" ]; then
  rc_file="${HOME}/.zshrc"
fi
if ! grep -q 'export GH_TOKEN=' "$rc_file" 2>/dev/null; then
  echo "" >> "$rc_file"
  echo '# GitHub CLI token' >> "$rc_file"
  echo "export GH_TOKEN=\"$gh_token_value\"" >> "$rc_file"
  echo "✓ GH_TOKEN persisted to $rc_file"
else
  echo "✓ GH_TOKEN already in $rc_file"
fi
echo ""
echo "Run the following for gh to pick up the token in this shell:"
echo "  source ~/.bashrc"

echo ""
echo "---"
echo "Go was installed to ~/go/bin. Run the following in your current shell:"
echo "  source ~/.bashrc"
echo "Or add ~/go/bin to PATH manually:"
echo "  export PATH=\"\$HOME/go/bin:\$PATH\""
echo "---"
echo ""

sync_apps "$@"
