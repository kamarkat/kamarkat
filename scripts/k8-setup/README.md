# Windows Development Environment Setup

This directory contains a PowerShell script that automatically installs and configures a complete development environment on Windows machines.

## What Gets Installed

- **Visual Studio Code** - Popular code editor
- **Git** - Version control system
- **Node.js** - JavaScript runtime and npm package manager
- **kubectl** - Kubernetes command-line tool
- **SSH Key** - Generates ed25519 SSH key for GitHub/GitLab
- **Git Configuration** - Sets up user name and email

## Quick Start

### Option 1: One-Click Setup (Recommended)

1. Right-click on `setup-windows.ps1`
2. Select "Run with PowerShell"
3. Follow the prompts (may need to bypass execution policy)

### Option 2: PowerShell Command

Open PowerShell as Administrator and run:

```powershell
Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope CurrentUser -Force
.\setup-windows.ps1
```

### Option 3: Download and Run

```powershell
# Download and run in one command
iwr -useb https://raw.githubusercontent.com/your-repo/kamarkat/main/scripts/k8-setup/setup-windows.ps1 | iex
```

## Script Details

### Prerequisites

- Windows 10/11
- PowerShell 5.1 or later
- Internet connection
- Administrator privileges (recommended)

### What the Script Does

1. **Sets Execution Policy** - Allows PowerShell scripts to run
2. **Installs VS Code** - Downloads and installs silently
3. **Installs Git** - Downloads latest Git for Windows
4. **Installs Node.js** - Downloads and installs LTS version
5. **Downloads kubectl** - Places kubectl.exe in user profile
6. **Generates SSH Key** - Creates ed25519 key pair for `arun4infra@gmail.com`
7. **Configures Git** - Sets user name and email globally
8. **Verifies Installation** - Shows version info for all tools

### SSH Key Information

The script generates an SSH key at:
- Private key: `%USERPROFILE%\.ssh\id_ed25519`
- Public key: `%USERPROFILE%\.ssh\id_ed25519.pub`

The public key content is displayed at the end of the script. **Copy this and add it to your GitHub/GitLab account.**

### Git Configuration

Sets the following global Git configuration:
- User name: `Arun Subramanian`
- User email: `arun4infra@gmail.com`
- Default branch: `main`
- Pull strategy: `merge` (not rebase)

## Troubleshooting

### Execution Policy Error

If you get "cannot be loaded because running scripts is disabled", run:

```powershell
Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope CurrentUser -Force
```

### Network Issues

If downloads fail, check:
- Internet connection
- Firewall/antivirus settings
- Corporate proxy configuration

### Permission Issues

Run PowerShell as Administrator if you encounter permission errors.

### Manual Installation

If the script fails, you can install manually:

1. **VS Code**: https://code.visualstudio.com/
2. **Git**: https://git-scm.com/download/win
3. **Node.js**: https://nodejs.org/
4. **kubectl**: Download from https://kubernetes.io/docs/tasks/tools/install-kubectl-windows/

## Verification

After installation, verify tools are working:

```powershell
code --version
git --version
node --version
npm --version
kubectl version --client
```

## Next Steps

1. Add your SSH public key to GitHub/GitLab
2. Clone your repositories
3. Start coding!

## Support

For issues with this script:
1. Check the troubleshooting section above
2. Verify your internet connection
3. Run PowerShell as Administrator
4. Ensure Windows is up to date

---

**Note**: This script is designed for Windows development environment setup. For macOS/Linux, different scripts would be needed.
