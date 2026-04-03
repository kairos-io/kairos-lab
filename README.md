# kairos-lab

`kairos-lab` is a small Go CLI for a first-time local Kairos experience.

This project is not meant to replace virtualization software like virt-manager or UTM. It's aimed at users who don't run virtualization software in their day-to-day and want to give Kairos a try.

The second goal of this project is to help the Kairos team deliver workshops and keep the focus on topics related to Kairos, not on the glitches between different host operating systems or different virtualization software out there.

After you've played with kairos-lab, whether you choose to continue your Kairos journey or not, you can run the `cleanup` command to remove any configuration, downloaded packages, or ISO images.

It helps you:

- download a Kairos ISO (`download`)
- boot a Kairos VM with bridged networking (`start`)
- manage multiple VM disks
- inspect state (`status`)
- clean VM artifacts (`reset`)
- clean everything created by the tool (`cleanup`)

## Supported Platforms

- macOS
- Linux


Windows is not supported, use your preferred virtualization software to spin up a Kairos VM e.g. VirtualBox. You might be able to run inside WSL but it's not recommended because without KVM support the experience will be terribly slow.

## Install

### macOS (recommended)

```bash
brew tap kairos-io/kairos
brew install kairos-lab
```

### Download Binary

Pre-built binaries are available on the [releases page](https://github.com/kairos-io/kairos-lab/releases).

**Note for macOS:** The binary is not signed. You'll need to authorize it in System Settings > Privacy & Security after the first run. The exact steps vary by macOS version.

### Build from Source

```bash
go build -o kairos-lab ./cmd/kairos-lab
```

## Quick Start

### 1) Setup dependencies (optional)

```bash
./kairos-lab setup
```

Detects your package manager and installs required tools (`qemu`) if missing.

### 2) Download a Kairos ISO

```bash
./kairos-lab download
```

Interactive selection of:
- Image type: `core` (base OS) or `standard` (with K3s)
- K3s version (if standard)

The ISO is saved to the cache directory and tracked for cleanup.

### 3) Start a VM

```bash
./kairos-lab start
```

This will:
- Create a new disk (named after the ISO + timestamp)
- Boot the VM with the ISO attached
- Use bridged networking (VM gets a LAN IP you can SSH to)
- Open a graphical window

**Exit the VM with `Ctrl-a x`**

### 4) Boot an installed system

After installing Kairos to the disk, start again:

```bash
./kairos-lab start
```

Select your existing disk - it will boot from disk without the ISO.

## Commands

### `download`

Downloads a Kairos ISO with interactive selection:
- Fetches latest release from GitHub
- Filters by your architecture (amd64/arm64)
- Prompts for core vs standard, K3s version

### `start`

Boots a VM with sensible defaults:
- **Display**: `window` (graphical) by default
- **Network**: `bridged` by default (VM gets LAN IP)
- **Disk**: Select existing or create new

Flags:
- `-name <name>` - Use/create disk with specific name
- `-new` - Force create new disk
- `-no-iso` - Boot without ISO (installed system)
- `-iso <path>` - Use specific ISO file
- `-display serial|window` - Display mode (default: window)
- `-network bridged|user` - Network mode (default: bridged)
- `-disk-size 60G` - Disk size for new disks
- `-memory 4096` / `-cpus 2` - VM resources
- `-yes` - Auto-confirm prompts

### `status`

Shows current state:
- Platform and dependencies
- Downloaded ISOs
- Disks and their associated ISOs
- Network configuration
- Running VM info

### `reset`

Removes VM artifacts:
- Disks (all or specific with `--disk <name>`)
- Network configuration
- Keeps downloaded ISOs and setup

### `cleanup`

Removes everything created by `kairos-lab`:
- All disks and runtime files
- Downloaded ISOs
- Network configuration
- Dependencies installed by the tool (not pre-existing ones)

## Networking

### macOS

Uses QEMU's `vmnet-bridged` mode. Requires sudo for QEMU to access vmnet.

### Linux

Requires **NetworkManager** for bridged networking. The tool:
- Creates a bridge (`kairoslab0`) and tap device
- Enslaves your physical interface to the bridge
- VM gets DHCP from your LAN

If NetworkManager is not available, use `--network user` for port-forwarded access (SSH via `localhost:2222`).

## State and Paths

By default:
- Config/state: `$XDG_CONFIG_HOME/kairos-lab/` (or `~/.config/kairos-lab/`)
- Cache/artifacts: `$XDG_CACHE_HOME/kairos-lab/` (or `~/.cache/kairos-lab/`)

Override with environment variables:
- `KAIROS_LAB_CONFIG_DIR`
- `KAIROS_LAB_CACHE_DIR`

## Safety

- Cleanup only removes what the tool created
- Dependencies that existed before setup are never removed
- Network cleanup restores original interface connection
- Destructive operations require confirmation (use `-yes` to skip)
