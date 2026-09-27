# kairos-lab

`kairos-lab` is a small Go CLI for a first-time local Kairos experience.

This project is not meant to replace virtualization software like virt-manager or UTM. It's aimed at users who don't run virtualization software in their day-to-day and want to give Kairos a try.

The second goal of this project is to help the Kairos team deliver workshops and keep the focus on topics related to Kairos, not on the glitches between different host operating systems or different virtualization software out there.

After you've played with kairos-lab, whether you choose to continue your Kairos journey or not, you can run the `cleanup` command to remove any configuration, downloaded packages, or ISO images.

It helps you:

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

Shows current state:
- Platform and dependencies
- The ISO and disk path in use
- Network configuration, including the bridge and tap on Linux, where
  `shared` and `bridged` build them (on macOS QEMU's vmnet backend does the
  bridging and there are none to name)
- The VM's address - always printed, in every mode, reading `none` until one
  is known
- In `user` mode, the two forwarded host ports - 2222 for SSH, 8080 for the
  WebUI - since a SLIRP guest has no address on the host's network to show
  instead
- Running VM info

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
- Dependencies installed by the tool (not pre-existing ones)

## Networking

Three modes, picked with `-network`. This CLI starts one VM at a time
whichever you pick - what differs is what that guest can reach, and what can
reach it:

| Mode | Gets | Cannot |
|---|---|---|
| `shared` (default) | Internet, an address the host can reach, a subnet of its own | Be reached from other machines on your LAN |
| `bridged` | An address on your LAN that other machines can reach | Work reliably over Wi-Fi |
| `user` | Internet, for a single VM | Be reached by another VM, or form a cluster |

**shared** (the default) attaches no physical interface at all - it puts the
VM on a private NAT subnet instead. That's also why it works over Wi-Fi,
where `bridged` often can't: no guest frame leaves the host with a MAC the
access point never saw associate. The VM still gets a real address on that
subnet, not just forwarded ports. That subnet could carry more than one
guest, but this CLI does not claim a cluster: nothing here asks for a second
VM, and neither way around that is supported. `start` refuses outright while
this config dir's VM is already running, on either platform; and pointing a
second run at another config dir resolves the same bridge and tap names,
since no flag sets them - which on Linux is what the pre-flight in
`internal/vm` treats as stale and removes, taking the running VM's network
with it. The limit today is the CLI's, not the subnet's.

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
`ssh -p 2222 kairos@localhost` and `http://localhost:8080`. It needs no
privileges and no NetworkManager. SLIRP is a userspace NAT inside the QEMU
process, so the guest has no address on your network at all and those two
forwarded ports are the only way in. That is what keeps `user` out of any
cluster, and unlike the one-VM limit above it is the network's rather than
this CLI's - no change here would lift it.

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

Both `shared` and `bridged` require **NetworkManager**, and both build a
bridge (`kairoslab0`) and a tap device for the VM:
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

Override with environment variables:
- `KAIROS_LAB_CONFIG_DIR`
- `KAIROS_LAB_CACHE_DIR`

## Safety

- Cleanup only removes what the tool created
- Dependencies that existed before setup are never removed
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
