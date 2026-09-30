# kairos-lab

`kairos-lab` is a small Go CLI for a first-time local Kairos experience.

This project is not meant to replace virtualization software like virt-manager or UTM. It's aimed at users who don't run virtualization software in their day-to-day and want to give Kairos a try.

The second goal of this project is to help the Kairos team deliver workshops and keep the focus on topics related to Kairos, not on the glitches between different host operating systems or different virtualization software out there.

After you've played with kairos-lab, whether you choose to continue your Kairos journey or not, you can run the `cleanup` command to remove any configuration, downloaded packages, or ISO images.

It helps you:

- get an `auroraboot` command that builds Kairos ISOs from container images (`setup`)
- download a Kairos ISO (`download`)
- boot a Kairos VM with shared networking by default (`start`)
- manage multiple VM disks
- inspect state (`status`)
- clean VM artifacts (`reset`)
- clean everything created by the tool (`cleanup`)

> **Found a bug, or want to request a feature?** Open it on
> [kairos-io/kairos](https://github.com/kairos-io/kairos/issues), including
> issues about this repository. Every Kairos issue lives in one place, so you
> never have to work out which repository to file against.

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

### Linux

```bash
curl -sSL https://raw.githubusercontent.com/kairos-io/kairos-lab/main/install.sh | sh
```

Detects your architecture (amd64/arm64), downloads the matching release from
the [releases page](https://github.com/kairos-io/kairos-lab/releases),
verifies it against the release's `checksums.txt`, and installs the
`kairos-lab` binary onto `PATH` (`/usr/local/bin` if writable or via `sudo`
with your confirmation, otherwise `~/.local/bin`). It prints the version,
download URL, and install path, and asks for confirmation before making any
change; answering anything other than yes leaves your system untouched. See
[`install.sh`](install.sh) for what it does, or download it first and read it
before running:

```bash
curl -sSL -o install.sh https://raw.githubusercontent.com/kairos-io/kairos-lab/main/install.sh
sh install.sh
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
It also gives you an `auroraboot` command, so you can build your own ISO:

```bash
auroraboot build-iso --output ./build quay.io/kairos/alpine:3.21-core-amd64-generic-v3.7.2
```

The command is a small script in `~/.local/bin` that runs a pinned
[AuroraBoot](https://github.com/kairos-io/AuroraBoot) container image on Docker
or podman. See [`setup`](#setup) for what it installs and how to opt out.

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
- Use shared networking (VM gets a real address on a NAT subnet you can SSH to)
- Open a graphical window
- Poll for the VM's address for up to 45s. While the VM is running that ends
  one of three ways: a usable address prints a WebUI URL and an SSH command; a
  link-local one (169.254.x.x, what a guest assigns itself when no DHCP server
  answers it) prints the same two lines under a heading saying the address is
  link-local, with what to check, since those URLs will not reach the VM; and a
  poll that runs out of time says it has stopped looking. The VM keeps running
  in all three. Quit the VM before any of them and the poll simply stops,
  saying nothing

**Exit the VM with `Ctrl-a x`**

### 4) Boot an installed system

After installing Kairos to the disk, start again:

```bash
./kairos-lab start
```

Select your existing disk - it will boot from disk without the ISO.

## Commands

### `setup`

Checks for the tools kairos-lab needs, installs what is missing, and sets up the
`auroraboot` command:

- A container runtime is needed to run AuroraBoot. Docker is the default, and
  podman works too. If both are usable, `-runtime` picks one; otherwise the
  first one that works is used, Docker before podman. A runtime counts as
  usable when `<runtime> info` succeeds, and a `docker` that is really podman
  counts as podman.
- If the machine has no runtime at all, on Linux setup installs one (after
  asking, and asking again before it uses sudo): Docker by default, podman with
  `-runtime podman`. It never installs a runtime over one that is present but
  broken, and it says why it skipped. After installing Docker you still need to
  start the service and add yourself to the `docker` group. Setup prints the
  commands and changes neither itself.
- On macOS setup installs no runtime. With none present it prints a hint and
  skips the AuroraBoot step. Install Docker Desktop, Colima or podman and run
  `setup` again.
- Setup pulls the pinned image (`quay.io/kairos/auroraboot`, an exact tag, never
  `latest`) after asking, since it is about 2.2 GB. Declining skips this step
  and the rest of setup still completes.
- The shim is written to `~/.local/bin/auroraboot`. If that directory is not on
  your `PATH`, setup prints the `export PATH=...` line to add. It never edits
  your shell configuration.
- If an `auroraboot` that setup did not write is already on your `PATH` or at
  that path, setup leaves it alone and records nothing.

Flags:
- `-runtime docker|podman` - Container runtime for `auroraboot`
- `-no-auroraboot` - Do not provide the `auroraboot` command
- `-yes` - Auto-confirm prompts (installs, sudo and the image pull)

The shim runs the image with your current directory mounted at the same
absolute path and as the working directory, so relative paths in the arguments
mean the same thing inside the container. Any other existing host path named in
the arguments is mounted at its own path too, and an `--output` directory that
does not exist yet is created. Paths that would shadow system directories such
as `/etc` or `/usr` are refused. The arguments themselves are passed through
unchanged. Values given to `--set` are not scanned for paths, and
`AURORABOOT_*` environment variables are not passed into the container.

`build-iso` without `--output` currently writes the ISO inside the container,
where it is lost when the container exits, although AuroraBoot's help text says
current directory. Always pass `--output <dir>`; the shim prints a warning when
you forget. AuroraBoot's default is being changed, and the warning goes away
once the pinned image includes that change.

### `download`

Downloads a Kairos ISO with interactive selection:
- Fetches latest release from GitHub
- Filters by your architecture (amd64/arm64)
- Prompts for core vs standard, K3s version

### `start`

Boots a VM with sensible defaults:
- **Display**: `window` (graphical) by default
- **Network**: `shared` by default; see [Networking](#networking) for what
  each of the three modes can and can't reach
- **Disk**: Select existing or create new

Flags:
- `-name <name>` - Use/create disk with specific name
- `-new` - Force create new disk
- `-no-iso` - Boot without ISO (installed system)
- `-iso <path>` - Use specific ISO file
- `-display window|serial` - Display mode (default: window)
- `-network shared|bridged|user` - Network mode (default: shared)
- `-bridge-if <iface>` - Uplink for `bridged`, dropped if the run ends up in
  `shared` or `user` (default: resolved from the host's interfaces at run time)
- `-disk-size 60G` - Disk size for new disks
- `-memory <GB>` / `-cpus <n>` - VM resources (memory is in GB, not MB). Pass
  neither and a new disk gets 2 vCPUs and 8 GB of memory on Apple Silicon, 4 GB
  elsewhere, while an existing one reuses what it was last started with - or
  those same defaults, if it has none recorded
- `-yes` - Auto-confirm prompts

### `status`

Shows current state, with one block per VM this config dir knows about:
- Platform and dependencies
- For each VM: the ISO and disk path in use, its network mode, its own tap on
  Linux, its address (always printed, reading `none` until one is known), and
  whether it is running
- In `user` mode, that VM's own forwarded host ports - 2222 for SSH, 8080 for
  the WebUI at index 0, and 2222 plus its index / 8080 plus its index for a
  second or later VM - since a SLIRP guest has no address on the host's
  network to show instead
- The shared bridge, on Linux, where `shared` and `bridged` build one (on
  macOS QEMU's vmnet backend does the bridging and there is none to name),
  and every VM's own tap on it
- Any VM record this binary could not trust (a state.json field outside what
  it expects), named rather than silently dropped

### `reset`

Removes VM artifacts:
- Disks (all or specific with `-disk <name>`)
- Network configuration
- Keeps downloaded ISOs and setup

### `cleanup`

Removes everything created by `kairos-lab`:
- All disks and runtime files
- Downloaded ISOs
- Network configuration
- The `auroraboot` shim, and `~/.local/bin` if setup created it and it is empty
- The AuroraBoot image, if setup pulled it (an image that was already there is kept)
- Dependencies installed by the tool (not pre-existing ones), including a container runtime setup installed

## Networking

Three modes, picked with `-network`. `start` can now run more than one VM at
once in the same config dir (`-name` picks which); what a mode decides is
what that guest can reach, and what can reach it:

| Mode | Gets | Cannot |
|---|---|---|
| `shared` (default) | Internet, an address the host can reach, a subnet of its own | Be reached from other machines on your LAN |
| `bridged` | An address on your LAN that other machines can reach | Work reliably over Wi-Fi |
| `user` | Internet, for a single VM | Be reached by another VM, or form a cluster |

**shared** (the default) attaches no physical interface at all - it puts the
VM on a private NAT subnet instead. That's also why it works over Wi-Fi,
where `bridged` often can't: no guest frame leaves the host with a MAC the
access point never saw associate. The VM still gets a real address on that
subnet, not just forwarded ports, and that subnet really can carry more than
one guest now: two `start` runs in the same config dir, under different
`-name`s, get their own tap on the one shared bridge and can reach each
other and the internet, each with its own address. `start` still refuses a
second run under a name that is already live; what changed is that a
*different* name no longer gets refused, and no longer tears the first VM's
network down to make room for itself.

Two limits stay, and are worth being explicit about. **Multi-VM support is
per config dir, not host-wide**: index allocation and the liveness check
both look only within the one config dir a run is pointed at, while the
bridge itself is host-level, so a second config dir still resolves the same
default bridge and tap names and destroys the first's network, exactly as a
single-VM host always did - there is no flag that changes where a config dir
points, so this only bites when `KAIROS_LAB_CONFIG_DIR` is set to more than
one directory on the same host. And the shared segment is host-wide and
unisolated **between guests**, whatever config dir they came from: every VM
on it can see every other's traffic and can ARP- or DHCP-spoof a sibling.
Do not put anything on this subnet you would not put on the same LAN
segment as every other guest.

**bridged** puts the VM on your LAN with a real LAN address, at the cost of
enslaving a physical interface to the bridge. Bridging onto Wi-Fi is
unreliable by design, not a bug worth chasing: in the station-to-AP
direction, 802.11 uses a 3-address header whose source-address field is the
transmitter address, so a Wi-Fi client in normal (managed) mode has nowhere
to put the VM's own MAC, and the access point drops frames from a MAC that
never associated. 4-address/WDS mode is the exception, and it's
implementation-specific, which is why bridging onto Wi-Fi works on some
access points and fails on others.

**user** is QEMU's own NAT, with ports forwarded from the host - connect at
`ssh -p 2222 kairos@localhost` and `http://localhost:8080` for the first VM
you start. A second VM in `user` mode gets the next pair up, 2223/8081, and
so on - `kairos-lab status` shows which VM has which. It needs no
privileges and no NetworkManager. SLIRP is a userspace NAT inside the QEMU
process, so the guest has no address on your network at all and its own
forwarded ports are the only way in: that is what keeps `user` out of any
cluster, and it is the network's limit rather than this CLI's - no change
here would lift it.

### macOS

Both `shared` and `bridged` use QEMU's vmnet backend and need sudo: Apple
gates the vmnet entitlement to virtualization vendors, so a Homebrew QEMU can
reach it only when it's launched as root. `start` checks for that up front,
before anything is built (no disk image, no bridge, no tap), and refuses if
you can't get it - not in the admin or wheel group, or no sudo binary at all -
rather than fail midway through.

`bridged` only considers physical interfaces: tunnels (including a VPN's
`utun`), bridges, AirDrop and the other virtual devices are skipped whether
or not they hold the default route or have a link. Among what's left, the
interface holding the host's default route (`route -n get default`) is
preferred; otherwise it falls back to the first active one `ifconfig -l`
lists.

When nothing on the host qualifies, `start` refuses rather than bridge onto a
dead port, because vmnet builds the bridge anyway and the VM then boots with
no DHCP lease and no error; use `-network shared` or `-network user` instead.
Naming an interface yourself with `-bridge-if <iface>` skips that resolution,
but `start` still checks the interface you named has a link, and refuses if
it doesn't.

`start` prints a warning when the interface it ends up with - default-route,
fallback, or one you named - is a Wi-Fi radio, so you see that risk before the
VM boots rather than after it fails to get a lease. `shared` has no such
problem, since it attaches to no interface at all.

vmnet typically puts the shared subnet's gateway at `192.168.64.1/24`, and
the DHCP server behind it leases guests addresses above that. Treat the number
as an example, not a promise: the QEMU command line only ever asks for
`-netdev vmnet-shared,id=net0`, with no address options at all, so the guest
lands wherever Apple's vmnet framework decides to put it. Apple documents no
subnet policy for `VMNET_SHARED_MODE` - the maintainer of Apple's own
`container` project has called the assignment policy "completely
undocumented" - and a root-launched vmnet has reportedly landed on
`192.168.2.1/24` instead. Check `kairos-lab status`, or the address `start`
prints, for what your VM actually got.

### Linux

Both `shared` and `bridged` require **NetworkManager**, and both build one
bridge (`kairoslab0`) shared by every VM in this config dir, plus a tap
device of its own for each VM (`kairoslab-tap0` for the first, `kairoslab-tap1`
for the second, and so on):
- **shared** attaches nothing but the tap. NetworkManager assigns
  `10.42.x.1/24` to the bridge, then runs a DHCP server and NAT on it, so the
  VM gets an address on a private subnet with no physical interface touched.
  `x` increments only to avoid NetworkManager's own concurrently active
  shared reservations - a second shared connection gets `10.42.1.1/24` while
  the first keeps `10.42.0.1/24` - it is not conflict-detection against your
  existing network or routes. It's `10.42.0.1/24` when no other shared
  connection is active on the host. NetworkManager unmanages IPv4-shared
  connections when it stops, so a restart takes the bridge and tap down with
  it, and because these are created with autoconnect off they only come back
  on the next `start` - the trade for not running a DHCP server, DNS
  forwarder and NAT rule on every boot of a host that has no VM up at all.
- **bridged** also enslaves your physical interface to the bridge, so the VM
  takes its lease from your LAN instead. `-bridge-if` accepts a Wi-Fi device
  (`wlan*`) with no complaint, but the same Wi-Fi unreliability described
  above applies here too, and unlike macOS, nothing warns you before the VM
  boots. Its bridge carries `ipv4.method auto` rather than `shared`, so the
  unmanage-on-stop rule above never reaches it and a NetworkManager restart
  leaves it up; its connections autoconnect as well, so it comes back on its
  own if it ever does go down.

If NetworkManager is not available, use `-network user` for port-forwarded
access (`ssh -p 2222 kairos@localhost`, `http://localhost:8080`).

## State and Paths

By default:
- Config/state: `$XDG_CONFIG_HOME/kairos-lab/` (or `~/.config/kairos-lab/`)
- Cache/artifacts: `$XDG_CACHE_HOME/kairos-lab/` (or `~/.cache/kairos-lab/`)

The `auroraboot` command, when setup provides it, is `~/.local/bin/auroraboot`.

Override with environment variables:
- `KAIROS_LAB_CONFIG_DIR`
- `KAIROS_LAB_CACHE_DIR`

## Safety

- Cleanup only removes what the tool created
- Dependencies that existed before setup are never removed. That includes a
  container runtime and an AuroraBoot image that were there before setup: they
  are recorded as pre-existing and cleanup keeps them
- Cleanup removes the `auroraboot` shim only if it is a regular file that
  carries the line `# kairos-lab-managed: auroraboot shim`, so a script of your
  own with that name is never touched. It removes `~/.local/bin` only when
  setup created it and it is empty
- Under rootful Docker the container runs as root, so ISOs and other output
  written by `auroraboot` are owned by root. Remove them with `sudo`, or run
  podman rootless
- Network cleanup reconnects your physical interface after `bridged`, but
  only when it is what deleted the bridge-slave profile that had put the
  interface on the bridge: `nmcli device connect <iface>` activates whichever
  profile NetworkManager rates best for the device, and after a bridged run
  that is routinely the bridge-slave one, so reconnecting while it is still
  there would put the interface straight back on a bridge. When cleanup
  declines, it prints which interface it left alone and how to put it back
  yourself. When it does reconnect, NetworkManager may still pick a different
  profile than your original one. `shared` enslaves no interface, so there is
  nothing to reconnect
- Destructive operations require confirmation (use `-yes` to skip)
