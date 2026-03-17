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
	"sort"
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
		return runReset(args[1:], stdout, store)
	case "cleanup":
		return runCleanup(args[1:], stdin, stdout, store)
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

	fmt.Fprintln(stdout, "[1/4] Detecting platform and package manager")
	st, err := store.Load()
	if err != nil {
		return err
	}
	p := platform.Detect()
	st.Platform = state.Platform{OS: p.OS, Arch: p.Arch, PackageManager: p.PackageManager}

	fmt.Fprintln(stdout, "[2/4] Checking required dependencies")
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
		fmt.Fprintln(stdout, "[3/4] Installing missing dependencies")
		if err := deps.Install(p.PackageManager, pkgs, useSudo); err != nil {
			return err
		}
		st.Setup.InstalledByKairosLab = mergeUnique(st.Setup.InstalledByKairosLab, missingNames)
	} else {
		fmt.Fprintln(stdout, "[3/4] All dependencies already present")
	}

	fmt.Fprintln(stdout, "[4/4] Writing state")
	st.Setup.DependencyCheckPassed = true
	st.Setup.CompletedAt = state.NowRFC3339()
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "setup complete (%s/%s, pkg manager: %s)\n", p.OS, p.Arch, p.PackageManager)
	return nil
}

func runStart(args []string, stdin io.Reader, stdout, stderr io.Writer, store *state.Store) error {
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

	fmt.Fprintln(stdout, "[1/5] Resolving ISO source")
	downloadsDir := filepath.Join(store.CacheDir, "downloads")
	res, err := iso.Resolve(*isoPath, *isoURL, downloadsDir)
	if err != nil {
		return err
	}
	state.AddManagedDir(st, downloadsDir)
	if res.Downloaded {
		state.AddManagedFile(st, res.LocalPath)
	}

	fmt.Fprintln(stdout, "[2/5] Preparing directories and disk")
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
	if _, err := os.Stat(diskPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "Running: qemu-img create -f qcow2 %s %s\n", diskPath, *diskSize)
	}
	if err := vm.EnsureDisk(diskPath, *diskSize); err != nil {
		return err
	}
	state.AddManagedFile(st, diskPath)

	fmt.Fprintln(stdout, "[3/5] Preparing networking")
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

	biosPath := ""
	if runtime.GOOS == "darwin" {
		biosPath, err = macOSFirmwarePath()
		if err != nil {
			return err
		}
	}
	qgaSock := filepath.Join(runtimeDir, "kairos.sock")
	logPath := filepath.Join(runtimeDir, "qemu.log")
	binary, qemuArgs, err := vm.BuildQEMUCommand(vm.StartConfig{
		ISOPath:       res.LocalPath,
		DiskPath:      diskPath,
		QGASocketPath: qgaSock,
		CPUs:          *cpus,
		MemoryMB:      *memory,
		NetworkMode:   *network,
		BridgeIface:   *bridgeIface,
		LinuxTapName:  st.Network.TapName,
		MacOSBiosPath: biosPath,
	})
	if err != nil {
		return err
	}

	cmdName := binary
	cmdArgs := qemuArgs
	if runtime.GOOS == "darwin" && *network == "bridged" {
		ok, err := confirm(stdin, stdout, *autoYes, "bridged vmnet mode runs qemu with sudo")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sudo permission denied")
		}
		cmdName = "sudo"
		cmdArgs = append([]string{binary}, qemuArgs...)
	}

	fmt.Fprintln(stdout, "[4/5] Recording VM state")
	st.Network.Mode = *network
	st.Network.BridgeInterface = *bridgeIface
	st.VM.ISOSource = res.Source
	st.VM.ISOInput = res.Input
	st.VM.ISOLocal = res.LocalPath
	st.VM.DiskPath = diskPath
	st.VM.LogPath = logPath
	st.VM.QemuBinary = cmdName
	st.VM.QemuArgs = cmdArgs
	st.VM.StartedAt = state.NowRFC3339()
	st.VM.StoppedAt = ""
	st.VM.RuntimeDir = runtimeDir
	st.VM.QGASockPath = qgaSock
	st.VM.LastError = ""
	state.AddManagedFile(st, logPath)
	state.AddManagedFile(st, qgaSock)
	if err := store.Save(st); err != nil {
		return err
	}

	fmt.Fprintln(stdout, "[5/5] Starting VM console (attached)")
	fmt.Fprintf(stdout, "Running: %s\n", renderCommand(cmdName, cmdArgs))
	if *network == "user" {
		fmt.Fprintln(stdout, "user mode forwards: ssh localhost:2222, http localhost:8080")
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer func() {
		_ = logFile.Close()
	}()

	command := exec.Command(cmdName, cmdArgs...)
	if sf, ok := stdin.(*os.File); ok {
		command.Stdin = sf
	} else {
		command.Stdin = os.Stdin
	}
	if cmdName == "sudo" {
		// sudo on macOS may fail with "unable to allocate pty" when stdio is
		// proxied through pipes. Keep stdio attached directly to the terminal.
		command.Stdout = stdout
		command.Stderr = stderr
	} else {
		command.Stdout = io.MultiWriter(stdout, logFile)
		command.Stderr = io.MultiWriter(stderr, logFile)
	}
	if err := command.Start(); err != nil {
		st.VM.LastError = err.Error()
		_ = store.Save(st)
		return fmt.Errorf("start qemu: %w", err)
	}
	st.VM.PID = command.Process.Pid
	if err := store.Save(st); err != nil {
		_ = command.Process.Kill()
		return err
	}

	waitErr := command.Wait()
	st.VM.PID = 0
	st.VM.StoppedAt = state.NowRFC3339()
	if waitErr != nil {
		st.VM.LastError = waitErr.Error()
		_ = store.Save(st)
		return fmt.Errorf("vm exited with error: %w (log: %s)", waitErr, logPath)
	}
	st.VM.LastError = ""
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "vm exited")
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

	platformLabel := st.Platform.OS + "/" + st.Platform.Arch
	if st.Platform.OS == "" {
		platformLabel = p.OS + "/" + p.Arch + " (detected)"
	}
	pm := st.Platform.PackageManager
	if pm == "" {
		pm = p.PackageManager + " (detected)"
	}

	fmt.Fprintf(stdout, "platform: %s\n", platformLabel)
	fmt.Fprintf(stdout, "package manager: %s\n", pm)
	fmt.Fprintf(stdout, "dependencies present now: %s\n", joinOrNone(present))
	fmt.Fprintf(stdout, "dependencies pre-existing: %s\n", joinOrNone(st.Setup.PreExistingDeps))
	fmt.Fprintf(stdout, "dependencies installed by kairos-lab: %s\n", joinOrNone(st.Setup.InstalledByKairosLab))
	fmt.Fprintf(stdout, "managed dirs: %s\n", joinOrNone(st.ManagedDirs))
	fmt.Fprintf(stdout, "managed files: %s\n", joinOrNone(st.ManagedFiles))
	fmt.Fprintf(stdout, "iso source: %s\n", emptyAsNone(st.VM.ISOSource))
	fmt.Fprintf(stdout, "iso path: %s\n", emptyAsNone(st.VM.ISOLocal))
	fmt.Fprintf(stdout, "disk path: %s\n", emptyAsNone(st.VM.DiskPath))
	fmt.Fprintf(stdout, "network mode: %s\n", emptyAsNone(st.Network.Mode))
	if st.Network.Mode == "bridged" {
		fmt.Fprintf(stdout, "bridge iface: %s\n", emptyAsNone(st.Network.BridgeInterface))
		fmt.Fprintf(stdout, "bridge resources: bridge=%s tap=%s\n", emptyAsNone(st.Network.BridgeName), emptyAsNone(st.Network.TapName))
	}
	fmt.Fprintf(stdout, "vm running: %t\n", running)
	if running {
		fmt.Fprintf(stdout, "vm pid: %d\n", st.VM.PID)
	}
	if st.VM.LastError != "" {
		fmt.Fprintf(stdout, "last vm error: %s\n", st.VM.LastError)
	}
	return nil
}

func runReset(args []string, stdout io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would be removed")
	if err := fs.Parse(args); err != nil {
		return err
	}

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
	toRemove, toSkip := splitRemovalPaths(paths, st)

	printRemovalPlan(stdout, "reset", toRemove, toSkip)
	if *dryRun {
		fmt.Fprintln(stdout, "dry-run only, no changes made")
		return nil
	}

	for _, p := range toRemove {
		fmt.Fprintf(stdout, "Removing: %s\n", p)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		state.RemoveManagedFile(st, p)
	}

	st.VM = state.VM{}
	if err := store.Save(st); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "reset complete")
	return nil
}

func runCleanup(args []string, stdin io.Reader, stdout io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	autoYes := fs.Bool("yes", false, "auto-confirm destructive operations")
	dryRun := fs.Bool("dry-run", false, "show what would be removed")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Load()
	if err != nil {
		return err
	}

	pm := st.Platform.PackageManager
	pinfo := platform.Detect()
	if pm == "" {
		pm = pinfo.PackageManager
	}
	removeDeps := cleanup.DependenciesToRemove(st.Setup.PreExistingDeps, st.Setup.InstalledByKairosLab)
	required := deps.Required(pinfo)
	pkgRemovals := []string{}
	if len(removeDeps) > 0 && pm != "" {
		pkgRemovals, err = deps.UninstallablePackages(pm, removeDeps, required)
		if err != nil {
			return err
		}
	}

	filesToRemove, filesToSkip := splitRemovalPaths(st.ManagedFiles, st)
	dirsToRemove, dirsToSkip := splitRemovalPaths(st.ManagedDirs, st)

	fmt.Fprintln(stdout, "cleanup plan:")
	printList(stdout, "Will remove files", filesToRemove)
	printListWithReasons(stdout, "Will skip files", filesToSkip)
	printList(stdout, "Will remove directories", dirsToRemove)
	printListWithReasons(stdout, "Will skip directories", dirsToSkip)
	printList(stdout, "Will uninstall dependencies", pkgRemovals)
	printList(stdout, "Will keep dependencies (pre-existing)", st.Setup.PreExistingDeps)

	if runtime.GOOS == "linux" && st.Network.Mode == "bridged" {
		if st.Network.CreatedByKairosLab {
			printList(stdout, "Will remove bridged network resources", st.Network.CreatedResources)
		} else {
			printList(stdout, "Will keep bridged network resources", []string{"not created by kairos-lab"})
		}
	}

	if *dryRun {
		fmt.Fprintln(stdout, "dry-run only, no changes made")
		return nil
	}

	ok, err := confirm(stdin, stdout, *autoYes, "cleanup removes all kairos-lab artifacts and tool-installed dependencies")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cleanup cancelled")
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

	if len(pkgRemovals) > 0 && pm != "" {
		useSudo := runtime.GOOS == "linux"
		if useSudo {
			ok, err := confirm(stdin, stdout, *autoYes, "remove kairos-lab-installed dependencies with sudo")
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("cleanup cancelled")
			}
		}
		if err := deps.Uninstall(pm, pkgRemovals, useSudo); err != nil {
			return err
		}
	}

	for _, p := range filesToRemove {
		fmt.Fprintf(stdout, "Removing file: %s\n", p)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove file %s: %w", p, err)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirsToRemove)))
	for _, d := range dirsToRemove {
		fmt.Fprintf(stdout, "Removing directory: %s\n", d)
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove directory %s: %w", d, err)
		}
	}
	if err := store.RemoveStateFile(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(stdout, "cleanup complete")
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "kairos-lab: local Kairos workshop CLI")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  setup                Detect/install dependencies")
	fmt.Fprintln(w, "  start [flags]        Create disk and boot ISO in QEMU console")
	fmt.Fprintln(w, "  stop                 Stop running VM (fallback helper)")
	fmt.Fprintln(w, "  status               Show state and runtime information")
	fmt.Fprintln(w, "  reset [--dry-run]    Remove VM artifacts (keep setup)")
	fmt.Fprintln(w, "  cleanup              Remove everything created by tool")
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
	sort.Strings(out)
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
	sort.Strings(out)
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

func renderCommand(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, a := range args {
		if strings.ContainsAny(a, " \t\n\"") {
			parts = append(parts, fmt.Sprintf("%q", a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

func splitRemovalPaths(paths []string, st *state.State) ([]string, map[string]string) {
	remove := make([]string, 0, len(paths))
	skip := map[string]string{}
	seen := map[string]struct{}{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		if !cleanup.IsPathSafe(p, st.ManagedDirs) {
			skip[p] = "outside managed directories"
			continue
		}
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			skip[p] = "not found"
			continue
		}
		remove = append(remove, p)
	}
	sort.Strings(remove)
	return remove, skip
}

func printRemovalPlan(stdout io.Writer, label string, remove []string, skip map[string]string) {
	fmt.Fprintf(stdout, "%s plan:\n", label)
	printList(stdout, "Will remove", remove)
	printListWithReasons(stdout, "Will skip", skip)
}

func printList(w io.Writer, title string, values []string) {
	fmt.Fprintf(w, "- %s:\n", title)
	if len(values) == 0 {
		fmt.Fprintln(w, "  - none")
		return
	}
	for _, v := range values {
		fmt.Fprintf(w, "  - %s\n", v)
	}
}

func printListWithReasons(w io.Writer, title string, values map[string]string) {
	fmt.Fprintf(w, "- %s:\n", title)
	if len(values) == 0 {
		fmt.Fprintln(w, "  - none")
		return
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "  - %s (%s)\n", k, values[k])
	}
}

func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func emptyAsNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
