package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	BridgeIface   string
	LinuxTapName  string
	MacOSBiosPath string
	Detached      bool
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
			args = append(args, "-device", "virtio-gpu-pci")
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
	if cfg.NetworkMode == "bridged" {
		if cfg.LinuxTapName == "" {
			return "", nil, fmt.Errorf("bridged linux mode requires tap name")
		}
		args = append(args,
			"-netdev", "tap,id=net0,ifname="+cfg.LinuxTapName+",script=no,downscript=no",
			"-device", "virtio-net-pci,netdev=net0",
		)
	} else {
		args = append(args,
			"-netdev", "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080",
			"-device", "virtio-net-pci,netdev=net0",
		)
	}
	args = append(args,
		"-drive", "id=disk1,if=none,media=disk,file="+cfg.DiskPath,
		"-device", "virtio-blk-pci,drive=disk1,bootindex=0",
		"-drive", "id=cdrom1,if=none,media=cdrom,file="+cfg.ISOPath,
		"-device", "ide-cd,drive=cdrom1,bootindex=1",
		"-boot", "menu=on",
	)
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
	if cfg.BridgeIface == "" {
		cfg.BridgeIface = "en0"
	}
	args := []string{
		"-machine", "virt,accel=hvf,highmem=on",
		"-cpu", "host",
		"-smp", strconv.Itoa(cfg.CPUs),
		"-m", strconv.Itoa(cfg.MemoryMB),
		"-bios", cfg.MacOSBiosPath,
	}
	switch cfg.DisplayMode {
	case "serial":
		args = append(args, "-nographic", "-serial", "mon:stdio")
	case "window":
		args = append(args, "-display", "default", "-serial", "mon:stdio", "-device", "virtio-gpu-pci")
	default:
		return "", nil, fmt.Errorf("invalid display mode: %s", cfg.DisplayMode)
	}
	if cfg.NetworkMode == "bridged" {
		args = append(args,
			"-device", "virtio-net-pci,netdev=net0",
			"-netdev", "vmnet-bridged,id=net0,ifname="+cfg.BridgeIface,
		)
	} else {
		args = append(args,
			"-device", "virtio-net-pci,netdev=net0",
			"-netdev", "user,id=net0,hostfwd=tcp::2222-:22,hostfwd=tcp::8080-:8080",
		)
	}
	args = append(args,
		"-drive", "file="+cfg.DiskPath+",if=virtio,format=qcow2",
		"-cdrom", cfg.ISOPath,
		"-boot", "menu=on",
	)
	return binary, args, nil
}
