# kairos-lab

`kairos-lab` is a small Go CLI for a first-time local Kairos experience.

This project is not meant to replace virtualization software like virt-manager or UTM. It's aimed at users who don't run virtualization software in their day-to-day and want to give Kairos a try.

The second goal of this project is to help the Kairos team deliver workshops and keep the focus on topics related to Kairos, not on the glitches between different host operating systems or different virtualization software out there.

After you've played with kairos-lab, whether you choose to continue your Kairos journey or not, you can run the `cleanup` command to remove any configuration, downloaded packages, or ISO images.

It helps you:

- download a Kairos ISO (`download`)
- boot a Kairos VM with QEMU (**Linux: `--network auto` prefers wired‑LAN bridging when NetworkManager and an Ethernet default-route NIC exist; otherwise `virbr`, then slirp** — **Wi‑Fi is not bridged here**; **macOS defaults to `--network user`**)
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
- On **Linux**, **`--network auto`**: **`bridged`** only when NetworkManager runs and **`ip route` default routes expose a wired **Ethernet** uplink (**`wlp*` / Wi‑Fi client NICs are skipped**; DHCP is from **your LAN** on that wired segment — **172.x**, **192.168.x**, **10.x**). Otherwise **`virbr`** (**192.168.122.x**); otherwise **`user`**/slirp (**192.168.239.x**, **`localhost:2222`**, **`localhost:8080`**).

- Use **`user`** networking on macOS (**192.168.239.x**, SSH `localhost:2222`, HTTP `localhost:8080`)
- Open a graphical window

**Important:** slirp/virbr subnets are QEMU/libvirt-internal. **`bridged`/`auto`** only uses **Ethernet** bridge slaves (**not Wi‑Fi**). On Wi‑Fi-only laptops, **`auto`** falls through to **`virbr`**/**`user`** unless you plug in Ethernet (or USB–Ethernet tethering appearing as **`en*`**).

To skip LAN bridging explicitly: `./kairos-lab start --network virbr` (libvirt NAT) or `./kairos-lab start --network user` (pure slirp).

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
- **Network** (defaults): Linux **`auto`** (wired‑LAN **`bridged`** when NM + Ethernet default-route; else **`virbr`**; else **`user`/slirp**); macOS **`user`** (NAT; guest **192.168.239.0/24**, forwards **SSH `localhost:2222`**, **HTTP `localhost:8080`**)
- **Disk**: Select existing or create new

Flags:
- `-name <name>` - Use/create disk with specific name
- `-new` - Force create new disk
- `-no-iso` - Boot without ISO (installed system)
- `-iso <path>` - Use specific ISO file
- `-display serial|window` - Display mode (default: window)
- `-network auto|user|bridged|virbr` — Linux default **`auto`** (same‑LAN bridging **Ethernet only**, then **`virbr`**, then **`user`**). **`bridged`** / **`auto`** bridging **reject Wi‑Fi** (`wlan`/`wl*`) uplinks — use **`--bridge-if`** Ethernet, USB‑Ethernet tether, or **`--network virbr`/`user`** instead.
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

**Same IP space as your laptop:** only **wired LAN bridging** (Ethernet NIC slave to kairos‑lab **`kairoslab0`**) attaches the VM to DHCP on **that wired segment** (often **172.x**, **192.168.x**, **10.x**). **Wi‑Fi client interfaces (`wlp*` / `wlan`) are not bridged.** Slirp and **`virbr0`** NAT use unrelated private subnets.

### Linux `--network auto` (default)

1. **`bridged`** if **NetworkManager** is active **and** `ip route` default routes expose a wired **Ethernet** uplink (**not Wi‑Fi**).

2. Else **`virbr`** if **`virbr0`** is usable (typically **192.168.122.x**).

3. Else **`user`** (slirp **192.168.239.x**).

Use **`--network bridged`** with Ethernet (**`--bridge-if enpXsY`**). Wi‑Fi-only hosts usually want **`virbr`** (host reaches guest IP on **122.x**) or **`user`** (`localhost` forwards).


### QEMU user/slirp (`user` / auto fallback last)

QEMU **user** networking (slirp / NAT): the guest gets **`192.168.239.0/24`** (often **`.15`**) and can reach your host at **`192.168.239.2`**. That subnet exists **only inside the emulator** — your real machine does **not** have a route to the guest’s address, so **`ping 192.168.239.15` from the host will not work**, and **ICMP is not port-forwarded** anyway. **Host → guest** access is only via **forwarded TCP ports**: **SSH `ssh -p 2222 localhost`**, HTTP **`http://localhost:8080`**. For DHCP on **the same routed LAN cable segment** as nearby wired hosts on Linux, use **`--network bridged`** (**Ethernet uplink**, not **`wlp*`**).

### macOS

Bridged (`--network bridged`): QEMU's `vmnet-bridged` mode. Requires sudo for QEMU to access vmnet.

**User mode** (`--network user`): no sudo.

### Linux

**Bridged LAN** (`--network bridged`): requires **NetworkManager**, **Ethernet** uplink (**Wi‑Fi not supported**).

**NAT on virbr** (`--network virbr`, or **`--network auto`** when bridging isn’t plausible but **`virbr0`** works): QEMU uses TAP **`kairoslab-vtap0`** on **`virbr0`**. Typical: **`sudo virsh net-start default`**. TAP setup uses sudo (`ip tuntap` / `ip link`).

**Slirp / user NAT** (`--network user`, or last **`auto`** fallback): see [QEMU user/slirp](#qemu-userslirp-user--auto-fallback-last).

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
