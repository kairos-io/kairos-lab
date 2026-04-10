package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kairos-io/kairos-lab/internal/cleanup"
	"github.com/kairos-io/kairos-lab/internal/deps"
	"github.com/kairos-io/kairos-lab/internal/iso"
	"github.com/kairos-io/kairos-lab/internal/platform"
	"github.com/kairos-io/kairos-lab/internal/state"
	"github.com/kairos-io/kairos-lab/internal/vm"
)

func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, version string) error {
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
	case "download":
		return runDownload(args[1:], stdin, stdout, store)
	case "start":
		return runStart(args[1:], stdin, stdout, stderr, store)
	case "status":
		return runStatus(stdout, store)
	case "reset":
		return runReset(args[1:], stdin, stdout, store)
	case "cleanup":
		return runCleanup(args[1:], stdin, stdout, store)
	case "version", "-v", "--version":
		writeLine(stdout, version)
		return nil
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

	writeLine(stdout, "[1/4] Detecting platform and package manager")
	st, err := store.Load()
	if err != nil {
		return err
	}
	p := platform.Detect()
	st.Platform = state.Platform{OS: p.OS, Arch: p.Arch, PackageManager: p.PackageManager}

	writeLine(stdout, "[2/4] Checking required dependencies")
	required := deps.Required(p)
	present := deps.PresentNames(required)
	missing := deps.Missing(required)
	st.Setup.PreExistingDeps = mergeUnique(st.Setup.PreExistingDeps, present)

	if len(missing) > 0 {
		if p.PackageManager == "" {
			return fmt.Errorf("missing dependencies and no package manager detected")
		}
		missingNames := depNames(missing)
		writef(stdout, "missing dependencies: %s\n", strings.Join(missingNames, ", "))
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
		writeLine(stdout, "[3/4] Installing missing dependencies")
		if err := deps.Install(p.PackageManager, pkgs, useSudo); err != nil {
			return err
		}
		st.Setup.InstalledByKairosLab = mergeUnique(st.Setup.InstalledByKairosLab, missingNames)
	} else {
		writeLine(stdout, "[3/4] All dependencies already present")
	}

	writeLine(stdout, "[4/4] Writing state")
	st.Setup.DependencyCheckPassed = true
	st.Setup.CompletedAt = state.NowRFC3339()
	if err := store.Save(st); err != nil {
		return err
	}
	writef(stdout, "setup complete (%s/%s, pkg manager: %s)\n", p.OS, p.Arch, p.PackageManager)
	return nil
}

func runDownload(args []string, stdin io.Reader, stdout io.Writer, store *state.Store) error {
	if len(args) > 0 {
		return fmt.Errorf("download does not accept arguments")
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
		return err
	}

	downloadsDir := filepath.Join(store.CacheDir, "downloads")
	localPath, err := iso.Download(iso.DownloadConfig{
		DownloadsDir: downloadsDir,
		Stdin:        stdin,
		Stdout:       stdout,
	})
	if err != nil {
		return err
	}

	state.AddManagedDir(st, downloadsDir)
	state.AddManagedFile(st, localPath)

	if err := store.Save(st); err != nil {
		return err
	}
	return nil
}

func createNewDisk(st *state.State, vmDir, name, size string, stdout io.Writer) (*state.Disk, error) {
	diskPath := filepath.Join(vmDir, name+".qcow2")
	writef(stdout, "Creating disk: %s (%s)\n", name, size)
	if err := vm.EnsureDisk(diskPath, size); err != nil {
		return nil, err
	}

	disk := state.Disk{
		Name:      name,
		Path:      diskPath,
		CreatedAt: state.NowRFC3339(),
		Size:      size,
	}
	state.AddDisk(st, disk)
	state.AddManagedFile(st, diskPath)
	return &disk, nil
}

// selectOrCreateDisk prompts user to select existing disk or create new one
// Returns: disk, isoPath (only if new disk created), error
func selectOrCreateDisk(st *state.State, vmDir, downloadsDir, diskSize string, stdin io.Reader, stdout io.Writer) (*state.Disk, string, error) {
	if len(st.Disks) == 0 {
		return nil, "", fmt.Errorf("no disks found")
	}

	writeLine(stdout, "Existing disks:")
	for i, d := range st.Disks {
		isoInfo := ""
		if d.ISOName != "" {
			isoInfo = fmt.Sprintf(" (from %s)", d.ISOName)
		}
		createdAt := d.CreatedAt
		if t, err := time.Parse(time.RFC3339, d.CreatedAt); err == nil {
			createdAt = t.Local().Format("2006-01-02 15:04")
		}
		writef(stdout, "  [%d] %s - created %s%s\n", i+1, d.Name, createdAt, isoInfo)
	}
	writef(stdout, "  [n] Create new disk\n")
	writeLine(stdout, "")
	writef(stdout, "Choice [1-%d or n]: ", len(st.Disks))

	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, "", fmt.Errorf("cancelled")
	}

	choice := strings.TrimSpace(strings.ToLower(line))

	if choice == "n" || choice == "new" {
		// User wants new disk - need to select ISO first
		writeLine(stdout, "")
		res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
		if err != nil {
			return nil, "", err
		}
		isoLocal := res.LocalPath
		isoBaseName := strings.TrimSuffix(filepath.Base(isoLocal), ".iso")
		generatedName := fmt.Sprintf("%s-%s", isoBaseName, state.NowTimestamp())
		disk, err := createNewDisk(st, vmDir, generatedName, diskSize, stdout)
		if err != nil {
			return nil, "", err
		}
		disk.ISOName = filepath.Base(isoLocal)
		return disk, isoLocal, nil
	}

	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 1 || idx > len(st.Disks) {
		return nil, "", fmt.Errorf("invalid choice: %s", choice)
	}

	// Existing disk selected - no ISO needed
	return &st.Disks[idx-1], "", nil
}

func runStart(args []string, stdin io.Reader, stdout, stderr io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	isoPath := fs.String("iso", "", "path to ISO file")
	diskName := fs.String("name", "", "disk name (creates new if doesn't exist)")
	diskSize := fs.String("disk-size", "60G", "disk image size for new disks")
	newDisk := fs.Bool("new", false, "create a new disk (even if others exist)")
	noISO := fs.Bool("no-iso", false, "boot without ISO (for installed systems)")
	memory := fs.Int("memory", defaultMemoryMB(), "memory in MB")
	cpus := fs.Int("cpus", 2, "number of vCPUs")
	network := fs.String("network", "bridged", "network mode: bridged|user")
	display := fs.String("display", "window", "display mode: window|serial")
	bridgeIface := fs.String("bridge-if", defaultBridgeIface(), "bridge interface (macOS vmnet or Linux uplink iface)")
	autoYes := fs.Bool("yes", false, "auto-confirm sudo operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *network != "bridged" && *network != "user" {
		return fmt.Errorf("invalid network mode: %s", *network)
	}
	if *display != "serial" && *display != "window" {
		return fmt.Errorf("invalid display mode: %s", *display)
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
		return err
	}
	running, _ := vm.IsRunning(st.VM.PID)
	if running {
		return fmt.Errorf("a vm is already running with pid %d", st.VM.PID)
	}

	vmDir := filepath.Join(store.CacheDir, "vm")
	runtimeDir := filepath.Join(store.CacheDir, "runtime")
	downloadsDir := filepath.Join(store.CacheDir, "downloads")
	if err := os.MkdirAll(vmDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return err
	}
	state.AddManagedDir(st, vmDir)
	state.AddManagedDir(st, runtimeDir)
	state.AddManagedDir(st, downloadsDir)

	// Resolve disk and ISO
	var disk *state.Disk
	var isoLocal string

	// Handle explicit -iso flag
	if *isoPath != "" {
		res, err := iso.ResolveForStart(*isoPath, downloadsDir, stdin, stdout)
		if err != nil {
			return err
		}
		isoLocal = res.LocalPath
	}

	if *diskName != "" {
		// User specified a disk name
		disk = state.FindDiskByName(st, *diskName)
		if disk == nil {
			// Create new disk with this name - needs ISO
			if isoLocal == "" {
				res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
				if err != nil {
					return err
				}
				isoLocal = res.LocalPath
			}
			disk, err = createNewDisk(st, vmDir, *diskName, *diskSize, stdout)
			if err != nil {
				return err
			}
			disk.ISOName = filepath.Base(isoLocal)
		}
	} else if *newDisk || len(st.Disks) == 0 {
		// Create new disk (forced or no disks exist) - needs ISO
		if isoLocal == "" {
			res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
			if err != nil {
				return err
			}
			isoLocal = res.LocalPath
		}

		// Generate disk name from ISO
		isoBaseName := strings.TrimSuffix(filepath.Base(isoLocal), ".iso")
		generatedName := fmt.Sprintf("%s-%s", isoBaseName, state.NowTimestamp())
		disk, err = createNewDisk(st, vmDir, generatedName, *diskSize, stdout)
		if err != nil {
			return err
		}
		disk.ISOName = filepath.Base(isoLocal)
	} else {
		// Existing disks available - ask what to do
		var err error
		disk, isoLocal, err = selectOrCreateDisk(st, vmDir, downloadsDir, *diskSize, stdin, stdout)
		if err != nil {
			return err
		}
	}

	// Apply -no-iso flag (user explicitly doesn't want ISO even for new disk)
	if *noISO {
		isoLocal = ""
	}

	// Determine network interface for bridged mode
	networkIface := *bridgeIface
	if *network == "bridged" && runtime.GOOS == "linux" && networkIface == "" {
		candidates := vm.DetectUplinkCandidates()
		if len(candidates) == 0 {
			return fmt.Errorf("no suitable uplink interface found for bridged networking (use -bridge-if to specify one, or -network user for port-forwarded access)")
		} else if len(candidates) == 1 {
			networkIface = candidates[0]
		} else {
			// Multiple candidates - will prompt in config review
			networkIface = candidates[0] // default to first, user can change
		}
	}

	// Build VM configuration
	vmConfig := &vmStartConfig{
		DiskName:    disk.Name,
		DiskPath:    disk.Path,
		DiskSize:    disk.Size,
		ISOPath:     isoLocal,
		MemoryMB:    *memory,
		CPUs:        *cpus,
		NetworkMode: *network,
		NetworkIface: networkIface,
		Display:     *display,
	}

	// Show configuration and allow editing runtime settings
	if !*autoYes {
		var err error
		vmConfig, err = reviewVMConfig(vmConfig, stdin, stdout)
		if err != nil {
			return err
		}
		*memory = vmConfig.MemoryMB
		*cpus = vmConfig.CPUs
		*network = vmConfig.NetworkMode
		networkIface = vmConfig.NetworkIface
		*display = vmConfig.Display
	}

	writeLine(stdout, "")
	writeLine(stdout, "A VM will start and attach to this terminal.")
	writeLine(stdout, "To exit the VM, press: Ctrl-a x")
	writeLine(stdout, "")
	if !*autoYes {
		writef(stdout, "Press Enter to start (or Ctrl-c to cancel): ")
		var buf [1]byte
		if _, err := stdin.Read(buf[:]); err != nil {
			return fmt.Errorf("cancelled")
		}
		if buf[0] != '\n' && buf[0] != '\r' {
			return fmt.Errorf("cancelled")
		}
		writeLine(stdout, "")
	}

	writeLine(stdout, "[1/3] Preparing networking")
	if *network == "bridged" && runtime.GOOS == "linux" {
		st.Network.BridgeInterface = networkIface
		ok, err := confirm(stdin, stdout, *autoYes, fmt.Sprintf("bridged networking needs sudo to prepare bridge/tap (uplink: %s)", st.Network.BridgeInterface))
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
	// Use short names for socket (Unix socket path limit is ~108 chars)
	qgaSock := filepath.Join(runtimeDir, "qemu.sock")
	logPath := filepath.Join(runtimeDir, "qemu.log")
	binary, qemuArgs, err := vm.BuildQEMUCommand(vm.StartConfig{
		ISOPath:       isoLocal,
		DiskPath:      disk.Path,
		QGASocketPath: qgaSock,
		CPUs:          *cpus,
		MemoryMB:      *memory,
		NetworkMode:   *network,
		DisplayMode:   *display,
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

	writeLine(stdout, "[2/3] Recording VM state")
	st.Network.Mode = *network
	st.Network.BridgeInterface = networkIface
	st.VM.ISOLocal = isoLocal
	st.VM.DiskPath = disk.Path
	st.VM.DiskName = disk.Name
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

	writeLine(stdout, "[3/3] Starting VM")
	writef(stdout, "Running: %s\n", renderCommand(cmdName, cmdArgs))
	if *network == "user" {
		writeLine(stdout, "user mode forwards: ssh localhost:2222, http localhost:8080")
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
	writeLine(stdout, "vm exited")
	return nil
}

func runStatus(stdout io.Writer, store *state.Store) error {
	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
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

	writef(stdout, "platform: %s\n", platformLabel)
	writef(stdout, "package manager: %s\n", pm)
	writef(stdout, "dependencies present now: %s\n", joinOrNone(present))
	writef(stdout, "dependencies pre-existing: %s\n", joinOrNone(st.Setup.PreExistingDeps))
	writef(stdout, "dependencies installed by kairos-lab: %s\n", joinOrNone(st.Setup.InstalledByKairosLab))
	writef(stdout, "managed dirs: %s\n", joinOrNone(st.ManagedDirs))
	writef(stdout, "managed files: %s\n", joinOrNone(st.ManagedFiles))
	writef(stdout, "iso source: %s\n", emptyAsNone(st.VM.ISOSource))
	writef(stdout, "iso path: %s\n", emptyAsNone(st.VM.ISOLocal))
	writef(stdout, "disk path: %s\n", emptyAsNone(st.VM.DiskPath))
	writef(stdout, "network mode: %s\n", emptyAsNone(st.Network.Mode))
	if st.Network.Mode == "bridged" {
		writef(stdout, "bridge iface: %s\n", emptyAsNone(st.Network.BridgeInterface))
		writef(stdout, "bridge resources: bridge=%s tap=%s\n", emptyAsNone(st.Network.BridgeName), emptyAsNone(st.Network.TapName))
	}
	writef(stdout, "vm running: %t\n", running)
	if running {
		writef(stdout, "vm pid: %d\n", st.VM.PID)
	}
	if st.VM.LastError != "" {
		writef(stdout, "last vm error: %s\n", st.VM.LastError)
	}
	return nil
}

func runReset(args []string, stdin io.Reader, stdout io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "show what would be removed")
	autoYes := fs.Bool("yes", false, "auto-confirm destructive operations")
	diskToRemove := fs.String("disk", "", "remove specific disk by name (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
		return err
	}

	running, _ := vm.IsRunning(st.VM.PID)
	if running {
		return fmt.Errorf("a VM is still running (PID %d). Exit the VM first (Ctrl-a x in serial console)", st.VM.PID)
	}

	// Collect paths to remove
	var paths []string
	var disksToRemove []state.Disk

	if *diskToRemove != "" {
		// Remove specific disk
		disk := state.FindDiskByName(st, *diskToRemove)
		if disk == nil {
			return fmt.Errorf("disk not found: %s", *diskToRemove)
		}
		disksToRemove = append(disksToRemove, *disk)
		paths = append(paths, disk.Path)
	} else {
		// Remove all disks
		for _, d := range st.Disks {
			disksToRemove = append(disksToRemove, d)
			paths = append(paths, d.Path)
		}
	}

	// Add runtime files
	paths = append(paths, st.VM.LogPath, st.VM.QGASockPath)

	toRemove, toSkip := splitRemovalPaths(paths, st)

	writeLine(stdout, "reset plan:")
	if len(disksToRemove) > 0 {
		writeLine(stdout, "Will remove disks:")
		for _, d := range disksToRemove {
			writef(stdout, "  - %s\n", d.Name)
		}
	}
	printRemovalPlan(stdout, "reset", toRemove, toSkip)
	hasStaleNetwork := vm.HasStaleNetworkResources(st)
	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		printList(stdout, "Will clean up network resources", []string{
			"bridge: " + nonEmpty(st.Network.BridgeName, vm.DefaultBridgeName),
			"tap: " + nonEmpty(st.Network.TapName, vm.DefaultTapName),
		})
	} else if hasStaleNetwork {
		printList(stdout, "Will clean up stale network resources (from failed/interrupted setup)", []string{
			"bridge: " + vm.DefaultBridgeName,
			"connections: " + vm.DefaultBridgeName + ", " + vm.DefaultBridgeName + "-uplink, " + vm.DefaultBridgeName + "-tap",
		})
	}

	if *dryRun {
		writeLine(stdout, "dry-run only, no changes made")
		return nil
	}

	ok, err := confirm(stdin, stdout, *autoYes, "proceed with reset")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("reset cancelled")
	}

	for _, p := range toRemove {
		writef(stdout, "Removing: %s\n", p)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		state.RemoveManagedFile(st, p)
	}

	// Remove disks from state
	for _, d := range disksToRemove {
		state.RemoveDisk(st, d.Name)
	}

	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		writeLine(stdout, "Cleaning up bridged network...")
		if err := vm.CleanupLinuxBridge(st); err != nil {
			writef(stdout, "warning: bridge cleanup failed: %v\n", err)
		}
	} else if hasStaleNetwork {
		writeLine(stdout, "Cleaning up stale bridged network resources...")
		if err := vm.CleanupStaleNetworkResources(st); err != nil {
			writef(stdout, "warning: stale network cleanup failed: %v\n", err)
		}
	}

	st.VM = state.VM{}
	if err := store.Save(st); err != nil {
		return err
	}
	writeLine(stdout, "reset complete")
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
	if err := requireSetup(st); err != nil {
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

	writeLine(stdout, "cleanup plan:")
	printList(stdout, "Will remove files", filesToRemove)
	printListWithReasons(stdout, "Will skip files", filesToSkip)
	printList(stdout, "Will remove directories", dirsToRemove)
	printListWithReasons(stdout, "Will skip directories", dirsToSkip)
	printList(stdout, "Will uninstall dependencies", pkgRemovals)
	printList(stdout, "Will keep dependencies (pre-existing)", st.Setup.PreExistingDeps)

	hasStaleNetwork := vm.HasStaleNetworkResources(st)
	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		printList(stdout, "Will clean up network resources", []string{
			"bridge: " + nonEmpty(st.Network.BridgeName, vm.DefaultBridgeName),
			"tap: " + nonEmpty(st.Network.TapName, vm.DefaultTapName),
		})
	} else if hasStaleNetwork {
		printList(stdout, "Will clean up stale network resources (from failed/interrupted setup)", []string{
			"bridge: " + vm.DefaultBridgeName,
			"connections: " + vm.DefaultBridgeName + ", " + vm.DefaultBridgeName + "-uplink, " + vm.DefaultBridgeName + "-tap",
		})
	}

	if *dryRun {
		writeLine(stdout, "dry-run only, no changes made")
		return nil
	}

	ok, err := confirm(stdin, stdout, *autoYes, "cleanup removes all kairos-lab artifacts and tool-installed dependencies")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cleanup cancelled")
	}

	running, _ := vm.IsRunning(st.VM.PID)
	if running {
		return fmt.Errorf("a VM is still running (PID %d). Exit the VM first (Ctrl-a x in serial console)", st.VM.PID)
	}

	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		writeLine(stdout, "Cleaning up bridged network...")
		if err := vm.CleanupLinuxBridge(st); err != nil {
			writef(stdout, "warning: bridge cleanup failed: %v\n", err)
		}
	} else if hasStaleNetwork {
		writeLine(stdout, "Cleaning up stale bridged network resources...")
		if err := vm.CleanupStaleNetworkResources(st); err != nil {
			writef(stdout, "warning: stale network cleanup failed: %v\n", err)
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
		writef(stdout, "Removing file: %s\n", p)
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove file %s: %w", p, err)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirsToRemove)))
	for _, d := range dirsToRemove {
		writef(stdout, "Removing directory: %s\n", d)
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove directory %s: %w", d, err)
		}
	}
	if err := store.RemoveStateFile(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	writeLine(stdout, "cleanup complete")
	return nil
}

func printUsage(w io.Writer) {
	writeLine(w, "kairos-lab: local Kairos workshop CLI")
	writeLine(w, "")
	writeLine(w, "Quick start:")
	writeLine(w, "  kairos-lab download             Download a Kairos ISO")
	writeLine(w, "  kairos-lab start                Create disk and boot ISO")
	writeLine(w, "  kairos-lab start                Boot existing disk (after install)")
	writeLine(w, "")
	writeLine(w, "Commands:")
	writeLine(w, "  setup                Detect/install dependencies")
	writeLine(w, "  download             Download a Kairos ISO (interactive selection)")
	writeLine(w, "  start [flags]        Boot VM (select/create disk, optionally attach ISO)")
	writeLine(w, "  status               Show state and runtime information")
	writeLine(w, "  reset [--disk name]  Remove disks and network (keep setup/ISOs)")
	writeLine(w, "  cleanup              Remove everything created by tool")
	writeLine(w, "  version              Print CLI version")
	writeLine(w, "")
	writeLine(w, "Start flags:")
	writeLine(w, "  -name <name>         Use/create disk with this name")
	writeLine(w, "  -new                 Create new disk (even if others exist)")
	writeLine(w, "  -no-iso              Boot without ISO (installed system)")
	writeLine(w, "  -iso <path>          Use specific ISO file")
	writeLine(w, "")
	writeLine(w, "Exit VM with Ctrl-a x (QEMU serial console quit)")
}

func writeLine(w io.Writer, a ...any) {
	_, _ = fmt.Fprintln(w, a...)
}

func writef(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

func confirm(stdin io.Reader, stdout io.Writer, autoYes bool, msg string) (bool, error) {
	if autoYes {
		return true, nil
	}
	writef(stdout, "%s [y/N]: ", msg)
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

func prompt(stdin io.Reader, stdout io.Writer, msg string) (string, error) {
	writef(stdout, "%s: ", msg)
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return "", fmt.Errorf("no input")
		} else if !errors.Is(err, io.EOF) {
			return "", err
		}
	}
	return strings.TrimSpace(line), nil
}

type vmStartConfig struct {
	DiskName     string
	DiskPath     string
	DiskSize     string
	ISOPath      string
	MemoryMB     int
	CPUs         int
	NetworkMode  string
	NetworkIface string
	Display      string
}

func reviewVMConfig(cfg *vmStartConfig, stdin io.Reader, stdout io.Writer) (*vmStartConfig, error) {
	for {
		writeLine(stdout, "")
		writeLine(stdout, "VM Configuration:")
		writef(stdout, "     Disk:         %s\n", cfg.DiskName)
		writef(stdout, "     Disk path:    %s\n", cfg.DiskPath)
		writef(stdout, "     Disk size:    %s\n", cfg.DiskSize)
		if cfg.ISOPath != "" {
			writef(stdout, "     ISO:          %s\n", filepath.Base(cfg.ISOPath))
		} else {
			writeLine(stdout, "     ISO:          (none - booting from disk)")
		}
		writef(stdout, "  1) Memory:       %d MB\n", cfg.MemoryMB)
		writef(stdout, "  2) CPUs:         %d\n", cfg.CPUs)
		writef(stdout, "  3) Network:      %s\n", cfg.NetworkMode)
		if cfg.NetworkMode == "bridged" && runtime.GOOS == "linux" {
			writef(stdout, "  4) Net interface: %s\n", cfg.NetworkIface)
		}
		writef(stdout, "  5) Display:      %s\n", cfg.Display)
		writeLine(stdout, "")
		writef(stdout, "Press Enter to continue, or enter a number to edit: ")

		reader := bufio.NewReader(stdin)
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("cancelled")
		}
		line = strings.TrimSpace(line)

		if line == "" {
			return cfg, nil
		}

		choice := 0
		if _, err := fmt.Sscanf(line, "%d", &choice); err != nil {
			writeLine(stdout, "Invalid input, please enter a number or press Enter to continue")
			continue
		}

		switch choice {
		case 1:
			val, err := prompt(stdin, stdout, "Enter memory in MB (e.g., 4096, 8192)")
			if err != nil {
				return nil, err
			}
			if val != "" {
				var mb int
				if _, err := fmt.Sscanf(val, "%d", &mb); err == nil && mb > 0 {
					cfg.MemoryMB = mb
				} else {
					writeLine(stdout, "Invalid memory value")
				}
			}
		case 2:
			val, err := prompt(stdin, stdout, "Enter number of CPUs (e.g., 2, 4)")
			if err != nil {
				return nil, err
			}
			if val != "" {
				var cpus int
				if _, err := fmt.Sscanf(val, "%d", &cpus); err == nil && cpus > 0 {
					cfg.CPUs = cpus
				} else {
					writeLine(stdout, "Invalid CPU value")
				}
			}
		case 3:
			val, err := prompt(stdin, stdout, "Enter network mode (bridged or user)")
			if err != nil {
				return nil, err
			}
			if val == "bridged" || val == "user" {
				cfg.NetworkMode = val
			} else if val != "" {
				writeLine(stdout, "Invalid network mode, use 'bridged' or 'user'")
			}
		case 4:
			if cfg.NetworkMode == "bridged" && runtime.GOOS == "linux" {
				candidates := vm.DetectUplinkCandidates()
				if len(candidates) > 1 {
					writeLine(stdout, "Available interfaces:")
					for i, iface := range candidates {
						writef(stdout, "  %d) %s\n", i+1, iface)
					}
					val, err := prompt(stdin, stdout, fmt.Sprintf("Select interface [1-%d]", len(candidates)))
					if err != nil {
						return nil, err
					}
					var idx int
					if _, err := fmt.Sscanf(val, "%d", &idx); err == nil && idx >= 1 && idx <= len(candidates) {
						cfg.NetworkIface = candidates[idx-1]
					} else {
						writeLine(stdout, "Invalid selection")
					}
				} else {
					val, err := prompt(stdin, stdout, "Enter interface name")
					if err != nil {
						return nil, err
					}
					if val != "" {
						cfg.NetworkIface = val
					}
				}
			} else {
				writeLine(stdout, "Invalid option (network interface only available for bridged mode on Linux)")
			}
		case 5:
			val, err := prompt(stdin, stdout, "Enter display mode (window or serial)")
			if err != nil {
				return nil, err
			}
			if val == "window" || val == "serial" {
				cfg.Display = val
			} else if val != "" {
				writeLine(stdout, "Invalid display mode, use 'window' or 'serial'")
			}
		default:
			writeLine(stdout, "Invalid option")
		}
	}
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
	if runtime.GOOS == "linux" {
		return ""
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
	writef(stdout, "%s plan:\n", label)
	printList(stdout, "Will remove", remove)
	printListWithReasons(stdout, "Will skip", skip)
}

func printList(w io.Writer, title string, values []string) {
	writef(w, "- %s:\n", title)
	if len(values) == 0 {
		writeLine(w, "  - none")
		return
	}
	for _, v := range values {
		writef(w, "  - %s\n", v)
	}
}

func printListWithReasons(w io.Writer, title string, values map[string]string) {
	writef(w, "- %s:\n", title)
	if len(values) == 0 {
		writeLine(w, "  - none")
		return
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writef(w, "  - %s (%s)\n", k, values[k])
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

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

var errSetupRequired = errors.New("setup has not been completed. Please run 'kairos-lab setup' first")

func requireSetup(st *state.State) error {
	if !state.IsSetupComplete(st) {
		return errSetupRequired
	}
	return nil
}
