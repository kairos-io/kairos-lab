package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kairos-io/kairos-lab/internal/state"
)

type StartConfig struct {
	ISOPath       string
	DiskPath      string
	LogPath       string
	RuntimeDir    string
	QGASocketPath string
	CPUs          int
	MemoryMB      int
	NetworkMode   string
	DisplayMode   string
	// BridgeIface is the host interface to bridge onto. It is meaningful only
	// for "bridged" mode; "shared" attaches to no host interface and "user"
	// needs none, so both must leave it empty.
	BridgeIface  string
	LinuxTapName string
	// MACAddress is the guest NIC address, in the usual colon-separated hex
	// form. Leaving it empty (or whitespace-only) omits it from the command
	// line, so QEMU falls back to its own default. A non-empty value that is
	// not a well-formed MAC is an error, not a fallback: the realistic source
	// is a hand-edited or corrupted state file, and a silent fallback would
	// put two VMs on the same default address. Leading zeroes may be omitted
	// per octet; the address is padded to the form QEMU parses.
	MACAddress string
	// BiosPath is the UEFI firmware image. It is required on macOS (every
	// build) and on Linux arm64, where the virt machine carries no firmware
	// of its own; it is unused on Linux amd64, whose default machine comes
	// with SeaBIOS already.
	BiosPath string
	Detached bool
	// SSHPort and HTTPPort are the host-side forwards user-mode networking
	// publishes: 2222/8080 at index 0 (today's fixed defaults, unchanged) and
	// 2222+index/8080+index above it. Zero means "use the index-0 default", so
	// a StartConfig built before these fields existed keeps behaving exactly
	// as it did.
	SSHPort  int
	HTTPPort int
	// UserModeHostBind is the address user-mode's forwarded ports bind: ""
	// (INADDR_ANY, today's behaviour, unchanged) at index 0, since index 0
	// must not change what it already exposes, and "127.0.0.1" at every index
	// above it, which is new surface with no compatibility constraint to keep.
	UserModeHostBind string
}

// netDeviceArg builds the -device value for the guest NIC.
//
// A blank address -- unset, or nothing but whitespace -- is the documented
// graceful path: the bare device goes on the command line and QEMU falls back
// to its own default address, so a caller that never set the field still
// starts. Emitting a dangling "mac=" instead would be a command line QEMU
// rejects, which is why the blank check trims first; " " used to slip past it.
//
// Anything else that is not a MAC is an error, not a silent fallback. The
// value is pasted into a COMMA-SEPARATED QEMU option list, so
// "52:54:00:12:34:56,romfile=/tmp/evil.rom" would inject further device
// properties rather than set an address. From M5 the address is read out of
// the user's state.json, where a corrupted or hand-edited entry would
// otherwise surface as an opaque QEMU startup abort naming neither the MAC nor
// the file it came from; falling back to QEMU's default instead would hand the
// VM the colliding address MACForDisk exists to avoid. So: fail, and name the
// offending value.
//
// The address is emitted in CanonicalMAC's zero-padded form, which is what
// QEMU's parser wants -- never NormalizeMAC's zero-stripped comparison form.
// userModeNetdevArg builds the -netdev value for user-mode networking.
// SSHPort/HTTPPort default to 2222/8080 when unset (a StartConfig built
// before these fields existed keeps today's exact command line), and
// UserModeHostBind prefixes both forwards -- "" reproduces today's
// INADDR_ANY bind at index 0, and "127.0.0.1" is what internal/app sets for
// every index above it.
func userModeNetdevArg(cfg StartConfig) string {
	sshPort := cfg.SSHPort
	if sshPort == 0 {
		sshPort = 2222
	}
	httpPort := cfg.HTTPPort
	if httpPort == 0 {
		httpPort = 8080
	}
	bind := cfg.UserModeHostBind
	return fmt.Sprintf("user,id=net0,hostfwd=tcp:%s:%d-:22,hostfwd=tcp:%s:%d-:8080", bind, sshPort, bind, httpPort)
}

func netDeviceArg(mac string) (string, error) {
	const device = "virtio-net-pci,netdev=net0"
	if strings.TrimSpace(mac) == "" {
		return device, nil
	}
	canonical, ok := CanonicalMAC(mac)
	if !ok {
		return "", fmt.Errorf("invalid MAC address %q in the stored VM configuration: "+
			"expected six colon-separated hex octets, for example 52:54:00:12:34:56", mac)
	}
	return device + ",mac=" + canonical, nil
}

type Process struct {
	Binary string
	Args   []string
	PID    int
}

func EnsureDisk(diskPath, size string) error {
	if _, err := os.Stat(diskPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat disk path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		return fmt.Errorf("create disk directory: %w", err)
	}
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, size)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create disk image: %w: %s", err, string(out))
	}
	return nil
}

func BuildQEMUCommand(cfg StartConfig) (string, []string, error) {
	switch runtime.GOOS {
	case "linux":
		return buildLinux(cfg)
	case "darwin":
		return buildMacOS(cfg)
	default:
		return "", nil, fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
}

func Start(cfg StartConfig) (*Process, error) {
	binary, args, err := BuildQEMUCommand(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	logf, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	cmd := exec.Command(binary, args...)
	cmd.Stdout = logf
	cmd.Stderr = logf
	if cfg.Detached {
		cmd.Stdin = nil
	} else {
		cmd.Stdin = os.Stdin
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, fmt.Errorf("start qemu: %w", err)
	}
	_ = logf.Close()
	return &Process{Binary: binary, Args: args, PID: cmd.Process.Pid}, nil
}

func Stop(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, _ := IsRunning(pid)
		if !running {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = p.Signal(syscall.SIGKILL)
	return nil
}

// IsRunning reports whether pid names a live process.
//
// The three-way split on the signal-0 probe is deliberate: EPERM means the
// process exists and we merely may not signal it -- a root-recorded VM
// probed by a later unprivileged start, which KAIROS_LAB_CONFIG_DIR and the
// blessed root-writes-user's-state.json case make ordinary -- and that reads
// as running, not as absent. ESRCH is the one definite no. Anything else
// (the FindProcess error included, which on unix never itself fails for a
// bad pid but is kept here for the platforms where it can) is returned to
// the caller rather than swallowed into false: on unix, os.FindProcess
// always succeeds and does no probing of its own, so every real answer comes
// from the Signal call below, and "cannot tell" must not be collapsed into
// "not running" -- the two carry very different risk if acted on wrongly.
func IsRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	switch err := p.Signal(syscall.Signal(0)); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH), errors.Is(err, os.ErrProcessDone):
		return false, nil
	default:
		return false, err
	}
}

// VMIsLive reports whether a VM counts as live: its QEMU PID is running, or
// it has no such PID yet but was reserved by a starter process (StarterPID,
// written before command.Start() so the allocator and the sibling-aware
// preflight both see the reservation for the whole window that contains this
// VM's own privileged prepare) that is itself still running. A reservation
// with PID 0 has to count as live too, or two concurrent starts would both
// pick the same index before either QEMU process exists to check.
//
// An IsRunning that cannot tell resolves to live here: skipping a
// stale-cleanup step wrongly leaves a stale bridge for reset to clear, and
// tearing one down wrongly kills a running VM, and those costs are not
// symmetric. So both checks below treat an IsRunning error as "live" rather
// than propagate it -- there is no caller here that could act on the error
// differently anyway; every caller of VMIsLive just needs a bool.
func VMIsLive(v state.VM) bool {
	if v.PID > 0 {
		running, err := IsRunning(v.PID)
		if err != nil || running {
			return true
		}
	}
	if v.StarterPID > 0 {
		running, err := IsRunning(v.StarterPID)
		if err != nil || running {
			return true
		}
	}
	return false
}

func buildLinux(cfg StartConfig) (string, []string, error) {
	return buildLinuxFor(runtime.GOARCH, cfg)
}

// buildLinuxFor takes the architecture as an argument so the arm64 command
// line can be exercised from an amd64 test host.
func buildLinuxFor(goarch string, cfg StartConfig) (string, []string, error) {
	binary := "qemu-system-x86_64"
	if goarch == "arm64" {
		binary = "qemu-system-aarch64"
	}
	if cfg.DisplayMode == "" {
		cfg.DisplayMode = "serial"
	}
	args := []string{}
	switch goarch {
	case "amd64":
		args = append(args, "-enable-kvm", "-cpu", "host")
	case "arm64":
		// 1. qemu-system-aarch64 has no default machine: leaving out -machine
		// exits with "No machine specified, and there is no default", which
		// is system/vl.c's select_machine() emitting that message once its
		// call to find_default_machine() comes back NULL, before QEMU ever
		// reads the ISO. -bios is a separate requirement, not a partner in
		// that same failure: a command line with -machine but no -bios
		// starts and runs -- it just boots nothing, because the virt
		// machine's flash is empty (kairos-io/kairos#4858).
		//
		// 2. -enable-kvm and -cpu host are coupled here, not optional extras:
		// QEMU rejects -cpu host outside KVM/HVF, so this mirrors amd64's
		// accelerator policy deliberately rather than picking a TCG-safe CPU.
		// The consequence worth naming: on an arm64 Linux host with no
		// /dev/kvm (a guest under Lima/UTM, an unprivileged container) start
		// fails with a KVM error instead of quietly falling back to TCG --
		// that fallback would need -cpu max instead, and is not what this
		// restores.
		//
		// 3. The macOS sibling folds its accelerator into
		// "-machine virt,accel=hvf,highmem=on" (see buildMacOS) instead of a
		// separate flag. The two accelerator spellings are equivalent:
		// -enable-kvm is shorthand for accel=kvm merged into the same
		// machine-options dict, and "virt,gic-version=max" here sets no accel
		// key of its own, so the later -machine does not clear the one
		// -enable-kvm set.
		if cfg.BiosPath == "" {
			return "", nil, fmt.Errorf("missing qemu firmware path for arm64")
		}
		args = append(args,
			"-enable-kvm",
			"-machine", "virt,gic-version=max",
			"-cpu", "host",
			"-bios", cfg.BiosPath,
		)
	}
	switch cfg.DisplayMode {
	case "serial":
		args = append(args, "-nographic", "-serial", "mon:stdio")
	case "window":
		args = append(args, "-display", "default", "-serial", "mon:stdio")
		if goarch == "arm64" {
			args = append(args,
				"-device", "virtio-gpu-pci",
				"-device", "qemu-xhci",
				"-device", "usb-kbd",
				"-device", "usb-tablet",
			)
		}
	default:
		return "", nil, fmt.Errorf("invalid display mode: %s", cfg.DisplayMode)
	}
	args = append(args,
		"-m", strconv.Itoa(cfg.MemoryMB),
		"-smp", strconv.Itoa(cfg.CPUs),
		"-rtc", "base=utc,clock=rt",
		"-chardev", "socket,path="+cfg.QGASocketPath+",server=on,wait=off,id=qga0",
		"-device", "virtio-serial",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
	)
	// Every mode attaches the same NIC to netdev net0 and differs only in the
	// -netdev backend, so the device is built once here: one MAC validation,
	// one place for the two builders to stay identical.
	nic, err := netDeviceArg(cfg.MACAddress)
	if err != nil {
		return "", nil, err
	}
	switch cfg.NetworkMode {
	case "shared", "bridged":
		// On Linux both modes present the guest a tap device on a
		// NetworkManager bridge; only the bridge's own IPv4 method differs
		// (shared NATs, bridged takes a lease off the LAN), and that is
		// configured when the bridge is created, not here.
		if cfg.LinuxTapName == "" {
			return "", nil, fmt.Errorf("%s linux mode requires tap name", cfg.NetworkMode)
		}
		args = append(args,
			"-netdev", "tap,id=net0,ifname="+cfg.LinuxTapName+",script=no,downscript=no",
			"-device", nic,
		)
	default:
		// Any unknown or empty mode falls back to user networking rather than
		// leaving the guest with no NIC at all.
		args = append(args,
			"-netdev", userModeNetdevArg(cfg),
			"-device", nic,
		)
	}
	args = append(args,
		"-drive", "id=disk1,if=none,media=disk,file="+cfg.DiskPath,
		"-device", "virtio-blk-pci,drive=disk1,bootindex=0",
	)
	if cfg.ISOPath != "" {
		args = append(args,
			"-drive", "id=cdrom1,if=none,media=cdrom,file="+cfg.ISOPath,
			"-device", "ide-cd,drive=cdrom1,bootindex=1",
		)
	}
	args = append(args, "-boot", "menu=on")
	return binary, args, nil
}

func buildMacOS(cfg StartConfig) (string, []string, error) {
	if runtime.GOARCH != "arm64" {
		return "", nil, fmt.Errorf("macOS is only supported on Apple Silicon")
	}
	if cfg.DisplayMode == "" {
		cfg.DisplayMode = "serial"
	}
	binary := "qemu-system-aarch64"
	if cfg.BiosPath == "" {
		return "", nil, fmt.Errorf("missing macOS qemu firmware path")
	}
	if cfg.NetworkMode == "bridged" && cfg.BridgeIface == "" {
		// Defaulting to a hardcoded en0 here is how a VM ends up bridged onto
		// an unplugged port with no lease and no warning (kairos-io/kairos#4431).
		// The caller resolves the interface from the host's default route.
		return "", nil, fmt.Errorf("missing bridge interface for bridged networking")
	}
	args := []string{
		"-machine", "virt,accel=hvf,highmem=on",
		"-cpu", "host",
		"-smp", strconv.Itoa(cfg.CPUs),
		"-m", strconv.Itoa(cfg.MemoryMB),
		"-bios", cfg.BiosPath,
		// Same guest-agent trio as buildLinux, spelled identically: the IP
		// resolver M4 builds will read this socket, and without it that
		// discovery source will not exist on macOS at all.
		//
		// It will be the LAST resort there, though, behind the DHCP lease file
		// and the ARP cache: on macOS -- and only on macOS -- QEMU is launched
		// under sudo, for BOTH vmnet modes, bridged and shared
		// (internal/app/app.go, the vmnetNeedsSudo branch). vmnet-shared needs
		// root exactly as vmnet-bridged does, so the socket is root-owned in
		// either of them, and connecting to a unix socket needs write
		// permission: a resolver running as the user gets EACCES. Only user
		// mode escapes it. M4 should treat QGA on macOS as best-effort, not a
		// source to depend on.
		//
		// "virtio-serial" is an alias that qdev resolves to virtio-serial-pci
		// on QEMU_ARCH_ARM, so it is valid on the aarch64 virt machine.
		"-chardev", "socket,path=" + cfg.QGASocketPath + ",server=on,wait=off,id=qga0",
		"-device", "virtio-serial",
		"-device", "virtserialport,chardev=qga0,name=org.qemu.guest_agent.0",
	}
	switch cfg.DisplayMode {
	case "serial":
		args = append(args, "-nographic", "-serial", "mon:stdio")
	case "window":
		args = append(args,
			"-display", "default",
			"-serial", "mon:stdio",
			"-device", "virtio-gpu-pci",
			"-device", "qemu-xhci",
			"-device", "usb-kbd",
			"-device", "usb-tablet",
		)
	default:
		return "", nil, fmt.Errorf("invalid display mode: %s", cfg.DisplayMode)
	}
	// Same single NIC device as buildLinux, built once before the switch.
	nic, err := netDeviceArg(cfg.MACAddress)
	if err != nil {
		return "", nil, err
	}
	switch cfg.NetworkMode {
	case "shared":
		// No ifname here, ever: vmnet-shared attaches to no host interface by
		// design, NetdevVmnetSharedOptions has no ifname member, and the opts
		// visitor fails a leftover key with "Invalid parameter '%s'" -- so
		// passing one is a hard QEMU startup abort, not an ignored option.
		args = append(args,
			"-device", nic,
			"-netdev", "vmnet-shared,id=net0",
		)
	case "bridged":
		args = append(args,
			"-device", nic,
			"-netdev", "vmnet-bridged,id=net0,ifname="+cfg.BridgeIface,
		)
	default:
		// Any unknown or empty mode falls back to user networking rather than
		// leaving the guest with no NIC at all.
		args = append(args,
			"-device", nic,
			"-netdev", userModeNetdevArg(cfg),
		)
	}
	args = append(args,
		"-drive", "file="+cfg.DiskPath+",if=virtio,format=qcow2",
	)
	if cfg.ISOPath != "" {
		args = append(args, "-cdrom", cfg.ISOPath)
	}
	args = append(args, "-boot", "menu=on")
	return binary, args, nil
}
