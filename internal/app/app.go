package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kairos-io/kairos-lab/internal/cleanup"
	"github.com/kairos-io/kairos-lab/internal/deps"
	"github.com/kairos-io/kairos-lab/internal/iso"
	"github.com/kairos-io/kairos-lab/internal/platform"
	"github.com/kairos-io/kairos-lab/internal/state"
	"github.com/kairos-io/kairos-lab/internal/vm"
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	store, err := state.DefaultStore()
	if err != nil {
		return err
	}
	switch args[0] {
	case "setup":
		return runSetup(args[1:], stdin, stdout, stderr, store)
	case "start":
		return runStart(args[1:], stdin, stdout, stderr, store)
	case "stop":
		return runStop(args[1:], stdout, store)
	case "status":
		return runStatus(stdout, store)
	case "reset":
		return runReset(stdout, store)
	case "implode":
		return runImplode(args[1:], stdin, stdout, stderr, store)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command: %s", args[0])
	}
}

func runSetup(args []string, stdin io.Reader, stdout, _ io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	autoYes := fs.Bool("yes", false, "auto-confirm installs and sudo operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	p := platform.Detect()
	st.Platform = state.Platform{OS: p.OS, Arch: p.Arch, PackageManager: p.PackageManager}
	required := deps.Required(p)
	present := deps.PresentNames(required)
	missing := deps.Missing(required)

	st.Setup.PreExistingDeps = mergeUnique(st.Setup.PreExistingDeps, present)
	if len(missing) > 0 {
		if p.PackageManager == "" {
			return fmt.Errorf("missing dependencies and no package manager detected")
		}
		missingNames := depNames(missing)
		fmt.Fprintf(stdout, "missing dependencies: %s\n", strings.Join(missingNames, ", "))
		ok, err := confirm(stdin, stdout, *autoYes, "install missing dependencies now")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("dependency installation declined")
		}
		pkgs, err := deps.InstallablePackages(p.PackageManager, missing)
		if err != nil {
			return err
		}
		useSudo := p.OS == "linux"
		if useSudo {
			ok, err = confirm(stdin, stdout, *autoYes, "this step needs sudo to install packages")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("sudo permission denied")
			}
		}
		if err := deps.Install(p.PackageManager, pkgs, useSudo); err != nil {
			return err
		}
		st.Setup.InstalledByKairosLab = mergeUnique(st.Setup.InstalledByKairosLab, missingNames)
	}
	st.Setup.DependencyCheckPassed = true
	st.Setup.CompletedAt = state.NowRFC3339()
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "setup complete (%s/%s, pkg manager: %s)\n", p.OS, p.Arch, p.PackageManager)
	return nil
}

func runStart(args []string, stdin io.Reader, stdout, _ io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	isoPath := fs.String("iso", "", "path to local ISO")
	isoURL := fs.String("url", "", "URL to ISO")
	diskSize := fs.String("disk-size", "60G", "disk image size")
	memory := fs.Int("memory", defaultMemoryMB(), "memory in MB")
	cpus := fs.Int("cpus", 2, "number of vCPUs")
	network := fs.String("network", "bridged", "network mode: bridged|user")
	bridgeIface := fs.String("bridge-if", defaultBridgeIface(), "bridge interface (macOS vmnet only)")
	autoYes := fs.Bool("yes", false, "auto-confirm sudo operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network != "bridged" && *network != "user" {
		return fmt.Errorf("invalid network mode: %s", *network)
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	running, _ := vm.IsRunning(st.VM.PID)
	if running {
		return fmt.Errorf("a vm is already running with pid %d", st.VM.PID)
	}

	downloadsDir := filepath.Join(store.CacheDir, "downloads")
	res, err := iso.Resolve(*isoPath, *isoURL, downloadsDir)
	if err != nil {
		return err
	}
	state.AddManagedDir(st, downloadsDir)
	if res.Downloaded {
		state.AddManagedFile(st, res.LocalPath)
	}

	vmDir := filepath.Join(store.CacheDir, "vm")
	runtimeDir := filepath.Join(store.CacheDir, "runtime")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return err
	}
	state.AddManagedDir(st, vmDir)
	state.AddManagedDir(st, runtimeDir)

	diskPath := filepath.Join(vmDir, "kairos.qcow2")
	if err := vm.EnsureDisk(diskPath, *diskSize); err != nil {
		return err
	}
	state.AddManagedFile(st, diskPath)

	if *network == "bridged" && runtime.GOOS == "linux" {
		ok, err := confirm(stdin, stdout, *autoYes, "bridged networking needs sudo to create bridge/tap and dnsmasq")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sudo permission denied")
		}
		if err := vm.PrepareLinuxBridge(st, runtimeDir); err != nil {
			return err
		}
	}

	if *network == "bridged" && runtime.GOOS == "darwin" {
		ok, err := confirm(stdin, stdout, *autoYes, "bridged vmnet mode runs qemu with sudo")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sudo permission denied")
		}
	}

	qgaSock := filepath.Join(runtimeDir, "kairos.sock")
	logPath := filepath.Join(runtimeDir, "qemu.log")
	biosPath := ""
	if runtime.GOOS == "darwin" {
		biosPath, err = macOSFirmwarePath()
		if err != nil {
			return err
		}
	}
	proc, err := vm.Start(vm.StartConfig{
		ISOPath:       res.LocalPath,
		DiskPath:      diskPath,
		LogPath:       logPath,
		RuntimeDir:    runtimeDir,
		QGASocketPath: qgaSock,
		CPUs:          *cpus,
		MemoryMB:      *memory,
		NetworkMode:   *network,
		BridgeIface:   *bridgeIface,
		LinuxTapName:  st.Network.TapName,
		MacOSBiosPath: biosPath,
		Detached:      true,
	})
	if err != nil {
		return err
	}

	st.Network.Mode = *network
	st.Network.BridgeInterface = *bridgeIface
	st.VM.ISOSource = res.Source
	st.VM.ISOInput = res.Input
	st.VM.ISOLocal = res.LocalPath
	st.VM.DiskPath = diskPath
	st.VM.LogPath = logPath
	st.VM.QemuBinary = proc.Binary
	st.VM.QemuArgs = proc.Args
	st.VM.PID = proc.PID
	st.VM.StartedAt = state.NowRFC3339()
	st.VM.StoppedAt = ""
	st.VM.RuntimeDir = runtimeDir
	st.VM.QGASockPath = qgaSock
	state.AddManagedFile(st, logPath)
	state.AddManagedFile(st, qgaSock)

	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "vm started with pid %d\n", proc.PID)
	if *network == "user" {
		fmt.Fprintln(stdout, "user mode forwards: ssh localhost:2222, http localhost:8080")
	}
	return nil
}

func runStop(args []string, stdout io.Writer, store *state.Store) error {
	if len(args) > 0 {
		return fmt.Errorf("stop does not accept arguments")
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	running, _ := vm.IsRunning(st.VM.PID)
	if !running {
		fmt.Fprintln(stdout, "no running vm tracked")
		st.VM.PID = 0
		st.VM.StoppedAt = state.NowRFC3339()
		return store.Save(st)
	}
	if err := vm.Stop(st.VM.PID, 10*time.Second); err != nil {
		return err
	}
	st.VM.PID = 0
	st.VM.StoppedAt = state.NowRFC3339()
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "vm stopped")
	return nil
}

func runStatus(stdout io.Writer, store *state.Store) error {
	st, err := store.Load()
	if err != nil {
		return err
	}
	p := platform.Detect()
	req := deps.Required(p)
	present := deps.PresentNames(req)
	running, _ := vm.IsRunning(st.VM.PID)
	fmt.Fprintf(stdout, "platform: %s/%s\n", st.Platform.OS, st.Platform.Arch)
	if st.Platform.OS == "" {
		fmt.Fprintf(stdout, "platform: %s/%s (detected)\n", p.OS, p.Arch)
	}
	fmt.Fprintf(stdout, "package manager: %s\n", st.Platform.PackageManager)
	if st.Platform.PackageManager == "" {
		fmt.Fprintf(stdout, "package manager: %s (detected)\n", p.PackageManager)
	}
	fmt.Fprintf(stdout, "dependencies present now: %s\n", strings.Join(present, ", "))
	fmt.Fprintf(stdout, "dependencies pre-existing: %s\n", strings.Join(st.Setup.PreExistingDeps, ", "))
	fmt.Fprintf(stdout, "dependencies installed by kairos-lab: %s\n", strings.Join(st.Setup.InstalledByKairosLab, ", "))
	fmt.Fprintf(stdout, "managed dirs: %s\n", strings.Join(st.ManagedDirs, ", "))
	fmt.Fprintf(stdout, "managed files: %s\n", strings.Join(st.ManagedFiles, ", "))
	fmt.Fprintf(stdout, "iso source: %s\n", st.VM.ISOSource)
	fmt.Fprintf(stdout, "iso path: %s\n", st.VM.ISOLocal)
	fmt.Fprintf(stdout, "disk path: %s\n", st.VM.DiskPath)
	fmt.Fprintf(stdout, "network mode: %s\n", st.Network.Mode)
	if st.Network.Mode == "bridged" {
		fmt.Fprintf(stdout, "bridge iface: %s\n", st.Network.BridgeInterface)
		fmt.Fprintf(stdout, "bridge resources: bridge=%s tap=%s\n", st.Network.BridgeName, st.Network.TapName)
	}
	fmt.Fprintf(stdout, "vm running: %t\n", running)
	if running {
		fmt.Fprintf(stdout, "vm pid: %d\n", st.VM.PID)
	}
	return nil
}

func runReset(stdout io.Writer, store *state.Store) error {
	st, err := store.Load()
	if err != nil {
		return err
	}
	_ = vm.Stop(st.VM.PID, 5*time.Second)
	st.VM.PID = 0
	paths := []string{st.VM.DiskPath, st.VM.LogPath, st.VM.QGASockPath}
	if st.VM.ISOSource == "url" {
		paths = append(paths, st.VM.ISOLocal)
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if cleanup.IsPathSafe(p, st.ManagedDirs) {
			_ = os.Remove(p)
			state.RemoveManagedFile(st, p)
		}
	}
	st.VM = state.VM{}
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "vm artifacts removed (setup remains)")
	return nil
}

func runImplode(args []string, stdin io.Reader, stdout, _ io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("implode", flag.ContinueOnError)
	autoYes := fs.Bool("yes", false, "auto-confirm destructive operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Load()
	if err != nil {
		return err
	}
	if ok, err := confirm(stdin, stdout, *autoYes, "implode removes all kairos-lab artifacts and tool-installed dependencies"); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("implode cancelled")
	}
	_ = vm.Stop(st.VM.PID, 5*time.Second)

	if runtime.GOOS == "linux" && st.Network.Mode == "bridged" && st.Network.CreatedByKairosLab {
		ok, err := confirm(stdin, stdout, *autoYes, "remove bridged network resources with sudo")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("cannot guarantee safe cleanup without bridge removal")
		}
		if err := vm.CleanupLinuxBridge(st); err != nil {
			return err
		}
	}

	pm := st.Platform.PackageManager
	pinfo := platform.Detect()
	if pm == "" {
		pm = pinfo.PackageManager
	}
	removeDeps := cleanup.DependenciesToRemove(st.Setup.PreExistingDeps, st.Setup.InstalledByKairosLab)
	if len(removeDeps) > 0 && pm != "" {
		required := deps.Required(pinfo)
		pkgs, err := deps.UninstallablePackages(pm, removeDeps, required)
		if err != nil {
			return err
		}
		useSudo := runtime.GOOS == "linux"
		if useSudo {
			ok, err := confirm(stdin, stdout, *autoYes, "remove kairos-lab-installed dependencies with sudo")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("implode cancelled")
			}
		}
		if err := deps.Uninstall(pm, pkgs, useSudo); err != nil {
			return err
		}
	}

	_ = cleanup.RemoveManagedFiles(st)
	if err := cleanup.RemoveManagedDirs(st); err != nil {
		return err
	}
	if err := store.RemoveStateFile(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(stdout, "implode complete")
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "kairos-lab: local Kairos workshop CLI")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  setup                Detect/install dependencies")
	fmt.Fprintln(w, "  start [flags]        Create disk and boot ISO in QEMU")
	fmt.Fprintln(w, "  stop                 Stop running VM")
	fmt.Fprintln(w, "  status               Show state and runtime information")
	fmt.Fprintln(w, "  reset                Remove VM artifacts (keep setup)")
	fmt.Fprintln(w, "  implode              Remove everything created by tool")
}

func confirm(stdin io.Reader, stdout io.Writer, autoYes bool, prompt string) (bool, error) {
	if autoYes {
		return true, nil
	}
	fmt.Fprintf(stdout, "%s [y/N]: ", prompt)
	var answer string
	if _, err := fmt.Fscanln(stdin, &answer); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

func depNames(ds []deps.Dependency) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return out
}

func mergeUnique(a, b []string) []string {
	set := map[string]struct{}{}
	for _, v := range a {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	for _, v := range b {
		if v != "" {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func defaultMemoryMB() int {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return 8192
	}
	return 4096
}

func defaultBridgeIface() string {
	if runtime.GOOS == "darwin" {
		return "en0"
	}
	return ""
}

func macOSFirmwarePath() (string, error) {
	if runtime.GOOS != "darwin" {
		return "", nil
	}
	cmd := exec.Command("brew", "--prefix", "qemu")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("discover qemu brew prefix: %w", err)
	}
	prefix := strings.TrimSpace(string(out))
	path := filepath.Join(prefix, "share", "qemu", "edk2-aarch64-code.fd")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("firmware not found at %s", path)
	}
	return path, nil
}
