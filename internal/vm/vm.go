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
	MACAddress    string
	MacOSBiosPath string
	Detached      bool
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

func IsRunning(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false, nil
	}
	return true, nil
}

func buildLinux(cfg StartConfig) (string, []string, error) {
	binary := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		binary = "qemu-system-aarch64"
	}
	if cfg.DisplayMode == "" {
		cfg.DisplayMode = "serial"
	}
	args := []string{}
	if runtime.GOARCH == "amd64" {
		args = append(args, "-enable-kvm", "-cpu", "host")
	}
	switch cfg.DisplayMode {
	case "serial":
		args = append(args, "-nographic", "-serial", "mon:stdio")
	case "window":
		args = append(args, "-display", "default", "-serial", "mon:stdio")
		if runtime.GOARCH == "arm64" {
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
			"-netdev", "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080",
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
	if cfg.MacOSBiosPath == "" {
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
		"-bios", cfg.MacOSBiosPath,
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
			"-netdev", "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080",
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
