package app

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
		// status builds no flag set, so it has no NArg to check; it used to
		// ignore anything after the verb outright. See rejectPositionalArgs.
		if len(args) > 1 {
			return fmt.Errorf("unexpected argument %q: status takes no arguments", args[1])
		}
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

// rejectPositionalArgs fails a subcommand that was handed a positional
// argument. Every kairos-lab option is a flag, so a leftover argument is
// always a mistake, and it used to be a silent one: `kairos-lab start
// /path/to.iso` parsed the path into the flag set's remaining args and dropped
// it, so -iso stayed empty and iso.ResolveForStart auto-selected whatever was
// in the download cache. The VM booted from an image the user never named and
// nothing said so (kairos-io/kairos#4432).
//
// hint names the flag that carries the value the user probably meant, so the
// error points at the interface instead of only rejecting the input.
func rejectPositionalArgs(fs *flag.FlagSet, hint string) error {
	if fs.NArg() == 0 {
		return nil
	}
	if hint != "" {
		return fmt.Errorf("unexpected argument %q: %s", fs.Arg(0), hint)
	}
	return fmt.Errorf("unexpected argument %q: %s takes flags only", fs.Arg(0), fs.Name())
}

func runSetup(args []string, stdin io.Reader, stdout, _ io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	autoYes := fs.Bool("yes", false, "auto-confirm installs and sudo operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectPositionalArgs(fs, ""); err != nil {
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

// validDiskName returns an error if name contains path separators or other
// characters that would allow it to escape the vm directory when used as a filename.
func validDiskName(name string) error {
	if name == "" {
		return fmt.Errorf("disk name must not be empty")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("disk name must not contain path separators")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("disk name %q is not allowed", name)
	}
	return nil
}

// promptDiskName asks the user to confirm or replace suggestedName, re-prompting
// if the chosen name is already in takenNames or contains unsafe characters.
func promptDiskName(suggestedName string, takenNames map[string]struct{}, stdin io.Reader, stdout io.Writer) (string, error) {
	writeLine(stdout, "")
	writef(stdout, "Suggested disk name: %s\n", suggestedName)

	reader := bufio.NewReader(stdin)
	for {
		writef(stdout, "Press Enter to accept, or type a new name: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("cancelled")
		}
		name := strings.TrimSpace(line)
		if name == "" {
			name = suggestedName
		}
		if err := validDiskName(name); err != nil {
			writef(stdout, "Invalid name: %v\n", err)
			continue
		}
		if _, taken := takenNames[name]; taken {
			writef(stdout, "A disk named %q already exists, choose a different name.\n", name)
			continue
		}
		return name, nil
	}
}

// selectOrCreateDisk prompts the user to pick an existing disk or configure a new one.
// For new disks it returns a pending Disk struct (isNew=true) without creating any file.
func selectOrCreateDisk(st *state.State, vmDir, downloadsDir, diskSize string, stdin io.Reader, stdout io.Writer) (disk *state.Disk, isoLocal string, isNew bool, err error) {
	if len(st.Disks) == 0 {
		return nil, "", false, fmt.Errorf("no disks found")
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
		return nil, "", false, fmt.Errorf("cancelled")
	}

	choice := strings.TrimSpace(strings.ToLower(line))

	if choice == "n" || choice == "new" {
		writeLine(stdout, "")
		res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
		if err != nil {
			return nil, "", false, err
		}
		isoPath := res.LocalPath
		isoBaseName := strings.TrimSuffix(filepath.Base(isoPath), ".iso")
		suggestedName := fmt.Sprintf("%s-%s", isoBaseName, state.NowTimestamp())
		diskName, err := promptDiskName(suggestedName, diskNameSet(st), stdin, stdout)
		if err != nil {
			return nil, "", false, err
		}
		pending := &state.Disk{
			Name: diskName,
			Path: filepath.Join(vmDir, diskName+".qcow2"),
			Size: diskSize,
		}
		return pending, isoPath, true, nil
	}

	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 1 || idx > len(st.Disks) {
		return nil, "", false, fmt.Errorf("invalid choice: %s", choice)
	}

	return &st.Disks[idx-1], "", false, nil
}

// requireNetworkPrivilege is vm.RequireNetworkPrivilege behind a package-level
// var so that a test can put its own function in its place. It is the seam
// idiom this repo already uses for host-touching calls (see the note on
// IsLinuxBridge in internal/vm/network_stub.go), and it is needed here for a
// specific reason: the real function is a no-op everywhere except darwin, so
// on the Linux CI leg a direct call would return nil whether the wiring below
// existed or not. Every assertion about where the pre-flight sits -- that the
// mode it is asked about is the one the config review settled on, and that a
// refusal stops the run before a disk image is created -- would pass on a
// tree that had never called it at all.
var requireNetworkPrivilege = vm.RequireNetworkPrivilege

// prepareLinuxBridge is vm.PrepareLinuxBridge behind the same kind of seam,
// and for a reason of its own: runStart reads st.Network.BridgeInterface back
// after this call, because the prepare decides which interface is enslaved and
// can detect one itself. Nothing in a test can otherwise reach that read. The
// real function needs an active NetworkManager and issues sudo nmcli
// commands, so a test driving it for real would either fail on the CI host or
// reconfigure it, and stubbing vm.PrepareLinuxBridge is the only way to
// observe what runStart does with the answer it gives back.
var prepareLinuxBridge = vm.PrepareLinuxBridge

// prepareLinuxShared is vm.PrepareLinuxShared behind the same kind of seam,
// and it is here for the plainest reason of the three: without it no test
// reached the call at all. Replacing the call below with a comment left go
// build, go vet and the whole of go test ./... green -- the run that got
// nearest answered "n" at the sudo prompt just above it and stopped there --
// so the line that gives a Linux guest its network was pinned by nothing,
// while the bridged twin in the branch below it was pinned through a seam of
// its own. The real function needs an active NetworkManager and
// issues sudo nmcli commands, so a test that let it run would either fail on
// the CI host or leave a NAT bridge, a tap, a DHCP server and a masquerade
// rule behind on it. Standing in for it is the way to reach the call at all.
var prepareLinuxShared = vm.PrepareLinuxShared

// buildQEMUCommand is vm.BuildQEMUCommand behind the same kind of seam, and
// it is here for what it lets a test see rather than for what it lets a test
// avoid: the real function is safe to call on every CI leg. What is not safe
// -- or even possible, on the amd64 leg -- is reading BiosPath back out of it
// afterwards. internal/vm's own buildLinuxFor("amd64", ...) drops the field
// on the floor before it becomes a QEMU argument, because amd64's default
// machine already carries SeaBIOS, so the built command line looks identical
// whether firmwarePathFor resolved a real path or was never reached at all.
// Capturing the vm.StartConfig runStart built, at the call site and before
// BuildQEMUCommand has a chance to discard any of it, is the only way the
// amd64 CI leg can tell those two runs apart. (On macos-latest it is
// possible without this seam: buildMacOS emits "-bios", cfg.BiosPath
// directly, and app_darwin_test.go reads it back off the built command line
// with valueAfterArg.)
var buildQEMUCommand = vm.BuildQEMUCommand

// firmwareHostPlatform is runtime.GOOS/runtime.GOARCH behind a narrow seam
// that exists for one call below and nowhere else in this function: the
// network-mode resolution, vmnetNeedsSudo and every other read of the real
// host in runStart have to keep reading it, because disguising the host from
// those would be testing a run that could never happen -- a "linux" network
// decision sitting beside a "darwin" sudo prompt. The one decision that does
// need disguising is this one: firmwarePathFor's linux/arm64 branch is
// unreachable from an amd64 CI host through the real runtime.GOOS/GOARCH,
// which is the exact branch that shipped broken once already (see
// firmwarePathFor's own comment). This seam is what lets a test drive
// runStart end-to-end as though it were running on that host, for this one
// decision alone.
var firmwareHostPlatform = func() (string, string) { return runtime.GOOS, runtime.GOARCH }

func runStart(args []string, stdin io.Reader, stdout, stderr io.Writer, store *state.Store) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	isoPath := fs.String("iso", "", "path to ISO file")
	diskName := fs.String("name", "", "disk name (creates new if doesn't exist)")
	diskSize := fs.String("disk-size", "60G", "disk image size for new disks")
	newDisk := fs.Bool("new", false, "create a new disk (even if others exist)")
	noISO := fs.Bool("no-iso", false, "boot without ISO (for installed systems)")
	memory := fs.Int("memory", defaultMemoryMB()/1024, "memory in GB")
	cpus := fs.Int("cpus", 2, "number of vCPUs")
	network := fs.String("network", defaultNetworkMode, "network mode: "+strings.Join(networkModes, "|"))
	display := fs.String("display", defaultDisplayMode, "display mode: "+strings.Join(displayModes, "|"))
	bridgeIface := fs.String("bridge-if", defaultBridgeIface(), "bridge interface (macOS vmnet or Linux uplink iface)")
	autoYes := fs.Bool("yes", false, "auto-confirm sudo operations")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := rejectPositionalArgs(fs, "pass the ISO with -iso"); err != nil {
		return err
	}
	if !networkModeValid(*network) {
		return fmt.Errorf("invalid network mode: %s", *network)
	}
	if !displayModeValid(*display) {
		return fmt.Errorf("invalid display mode: %s", *display)
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
		return err
	}
	// The per-VM refusal replaces what used to be an early, single-VM check
	// here: which VM this start is about is not known until the disk is
	// resolved below, and the check that matters is made just above the
	// prepare blocks, right before the "[1/3] Preparing networking" step --
	// a prepare mutates the host before store.Save is reached. See the
	// reservation below.

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
	// New disks are not created yet — a pending Disk struct is built and the file
	// is only written to disk after the user confirms the full configuration.
	var disk *state.Disk
	var isoLocal string
	var isNewDisk bool

	// Handle explicit -iso flag
	if *isoPath != "" {
		res, err := iso.ResolveForStart(*isoPath, downloadsDir, stdin, stdout)
		if err != nil {
			return err
		}
		isoLocal = res.LocalPath
	}

	if *diskName != "" {
		disk = state.FindDiskByName(st, *diskName)
		if disk == nil {
			// Named disk doesn't exist yet — build a pending struct
			if isoLocal == "" {
				res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
				if err != nil {
					return err
				}
				isoLocal = res.LocalPath
			}
			pending := state.Disk{
				Name: *diskName,
				Path: filepath.Join(vmDir, *diskName+".qcow2"),
				Size: *diskSize,
			}
			disk = &pending
			isNewDisk = true
		}
	} else if *newDisk || len(st.Disks) == 0 {
		// New disk (forced or no disks exist) — build a pending struct
		if isoLocal == "" {
			res, err := iso.ResolveForStart("", downloadsDir, stdin, stdout)
			if err != nil {
				return err
			}
			isoLocal = res.LocalPath
		}
		isoBaseName := strings.TrimSuffix(filepath.Base(isoLocal), ".iso")
		suggestedName := fmt.Sprintf("%s-%s", isoBaseName, state.NowTimestamp())
		finalName := suggestedName
		if !*autoYes {
			var err error
			finalName, err = promptDiskName(suggestedName, diskNameSet(st), stdin, stdout)
			if err != nil {
				return err
			}
		}
		pending := state.Disk{
			Name: finalName,
			Path: filepath.Join(vmDir, finalName+".qcow2"),
			Size: *diskSize,
		}
		disk = &pending
		isNewDisk = true
	} else {
		// Existing disks available — ask what to do
		var err error
		disk, isoLocal, isNewDisk, err = selectOrCreateDisk(st, vmDir, downloadsDir, *diskSize, stdin, stdout)
		if err != nil {
			return err
		}
	}

	// Apply -no-iso flag (user explicitly doesn't want ISO even for new disk)
	if *noISO {
		isoLocal = ""
	}

	// Determine network interface for bridged mode. This is the first of two
	// resolutions: the flag's mode is not the final one, so the review below
	// resolves again for a run that arrives in another mode and leaves as
	// bridged. See resolveBridgeUplink.
	networkIface, err := resolveBridgeUplink(*network, *bridgeIface)
	if err != nil {
		return err
	}

	// For existing disks, seed memory/CPU from the disk's saved settings so
	// that the user's previous choices persist across restarts — but only when
	// the user did not explicitly pass -memory/-cpus on this invocation.
	if !isNewDisk {
		explicitFlags := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })
		if disk.MemoryGB > 0 && !explicitFlags["memory"] {
			*memory = disk.MemoryGB
		}
		if disk.CPUs > 0 && !explicitFlags["cpus"] {
			*cpus = disk.CPUs
		}
	}

	// Build VM configuration
	vmConfig := &vmStartConfig{
		DiskName:     disk.Name,
		DiskPath:     disk.Path,
		DiskSize:     disk.Size,
		ISOPath:      isoLocal,
		MemoryGB:     *memory,
		CPUs:         *cpus,
		NetworkMode:  *network,
		NetworkIface: networkIface,
		Display:      *display,
		DownloadsDir: downloadsDir,
		IsNewDisk:    isNewDisk,
		TakenNames:   diskNameSet(st),
	}

	// Show configuration and allow editing runtime settings
	if !*autoYes {
		var err error
		vmConfig, err = reviewVMConfig(vmConfig, stdin, stdout)
		if err != nil {
			return err
		}
		*memory = vmConfig.MemoryGB
		*cpus = vmConfig.CPUs
		*network = vmConfig.NetworkMode
		networkIface = vmConfig.NetworkIface
		*display = vmConfig.Display
		isoLocal = vmConfig.ISOPath

		// The review is the last writer of the mode, so the uplink is
		// resolved once more against what it settled on. A run that arrives
		// in any other mode -- which is every run that passes no -network at
		// all, since the flag defaults to shared -- skipped the resolution
		// above, and prompt 7 can still turn it into a bridged one. Without
		// this the interface stays empty: the sudo prompt a few steps down
		// would name nothing while vm.PrepareLinuxBridge went and detected an
		// uplink of its own, so the user would consent to enslaving an
		// interface nobody named.
		//
		// It is a second call and not a move, because a run that came in as
		// bridged has to see its interface on row 8 of the review it is being
		// shown. When that call already answered, this one returns the same
		// value without asking the host again.
		networkIface, err = resolveBridgeUplink(*network, networkIface)
		if err != nil {
			return err
		}
	}

	// Refuse a mode this host cannot give the privilege for, here and nowhere
	// else. Here, because the review above is the last writer of the mode --
	// a user who passed -network user and then answered "shared" at prompt 7
	// has to be told about shared -- and because nothing has been built yet:
	// no disk image, no bridge, no tap. Later would mean saying the run
	// cannot work after a 60 GB image exists, and the failure would arrive as
	// an opaque sudo password prompt followed by a vmnet error out of QEMU.
	// Nowhere else, because a second call at flag-parse time would print the
	// same refusal twice.
	if err := requireNetworkPrivilege(*network); err != nil {
		return err
	}

	// Resolved here, and not down by buildQEMUCommand where it used to sit,
	// because it is a precondition with no dependencies: firmwarePathFor
	// takes nothing but the (goos, goarch) pair firmwareHostPlatform reports,
	// so nothing computed between here and the old call site changes its
	// answer, and there is no reason to let anything privileged or
	// expensive happen first. The old spot sat after the shared- and
	// bridged-mode blocks below, which prompt for sudo and then have
	// vm.PrepareLinuxShared / vm.PrepareLinuxBridge build a NAT bridge and a
	// tap on the host -- in memory only, on the *st passed in, with
	// store.Save(st) -- the only place any of that gets written to disk --
	// not reached until well after both blocks return, down at the
	// "[2/3] Recording VM state" step below. A
	// linux/arm64 host missing EDK2 would sudo its way to a bridge and tap
	// and only then hit this error, leaving both on the host with nothing
	// in state.json to name them, so neither `kairos-lab reset` nor
	// `kairos-lab cleanup` could find them to tear them down. Checking it
	// here, before either prepare runs and before the disk is even
	// materialized, is the same reasoning requireNetworkPrivilege above
	// already applies to the network mode itself: refuse before anything
	// the refusal would have to leave behind.
	biosPath, err := firmwarePathFor(firmwareHostPlatform())
	if err != nil {
		return err
	}

	// Materialize disk — done after review so the config is final.
	if isNewDisk {
		// Validate the final disk path stays within vmDir before touching the filesystem.
		absVMDir, err := filepath.Abs(vmDir)
		if err != nil {
			return fmt.Errorf("resolve vm directory: %w", err)
		}
		absDiskPath, err := filepath.Abs(vmConfig.DiskPath)
		if err != nil {
			return fmt.Errorf("resolve disk path: %w", err)
		}
		rel, err := filepath.Rel(absVMDir, absDiskPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("disk path %q must be within vm directory %q", vmConfig.DiskPath, vmDir)
		}

		writef(stdout, "Creating disk: %s (%s)\n", vmConfig.DiskName, vmConfig.DiskSize)
		// Remove any stale file from a previous abandoned attempt so that the
		// user-chosen size is always honoured (EnsureDisk is a no-op when the
		// file already exists).
		if err := os.Remove(absDiskPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale disk image: %w", err)
		}
		if err := vm.EnsureDisk(absDiskPath, vmConfig.DiskSize); err != nil {
			return err
		}
		isoName := ""
		if isoLocal != "" {
			isoName = filepath.Base(isoLocal)
		}
		newDisk := state.Disk{
			Name:      vmConfig.DiskName,
			Path:      absDiskPath,
			CreatedAt: state.NowRFC3339(),
			Size:      vmConfig.DiskSize,
			ISOName:   isoName,
		}
		state.AddDisk(st, newDisk)
		state.AddManagedFile(st, vmConfig.DiskPath)
		disk = state.FindDiskByName(st, vmConfig.DiskName)
		if disk == nil {
			return fmt.Errorf("internal error: disk not found after creation")
		}
	} else if vmConfig.DiskSize != disk.Size {
		// Existing disk: size was changed (gate already confirmed during review)
		writef(stdout, "Recreating disk at new size %s...\n", vmConfig.DiskSize)
		if err := os.Remove(disk.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove old disk image: %w", err)
		}
		if err := vm.EnsureDisk(disk.Path, vmConfig.DiskSize); err != nil {
			return err
		}
		if stateDisk := state.FindDiskByName(st, disk.Name); stateDisk != nil {
			stateDisk.Size = vmConfig.DiskSize
		}
		disk.Size = vmConfig.DiskSize
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

	// The per-VM refusal, the reservation, and the index allocation, all in
	// one Update so the check, the allocation and the reservation are atomic
	// against a sibling doing the same thing at the same time -- without
	// that, two concurrent starts could both read "index free" and both pick
	// 0. This sits above both prepare blocks below and above every later
	// write of this VM's own state, because "[2/3] Recording VM state" is
	// not reached until well after a prepare has already mutated the host.
	//
	// disk.Name is used, not vmConfig.DiskName: the review above renames a
	// NEW disk only, and a new disk has no live VM by construction, so the
	// post-review name is the one to key the reservation on.
	vmName := disk.Name
	var vmIndex int
	var siblingLive bool
	reservedAt := state.NowRFC3339()
	if err := store.Update(func(s *state.State) error {
		if existing := state.FindVM(s, vmName); existing != nil && vm.VMIsLive(*existing) {
			return fmt.Errorf("a vm named %q is already running (pid %d)", vmName, existing.PID)
		}
		for _, v := range s.VMs {
			if v.Name != vmName && vm.VMIsLive(v) {
				siblingLive = true
			}
		}
		idx, err := state.NextFreeVMIndex(s, vm.VMIsLive)
		if err != nil {
			return err
		}
		vmIndex = idx
		state.UpsertVM(s, state.VM{
			Name:       vmName,
			Index:      idx,
			DiskName:   disk.Name,
			DiskPath:   disk.Path,
			StarterPID: os.Getpid(),
			StartingAt: reservedAt,
		})
		return nil
	}); err != nil {
		return err
	}
	// Releases the reservation on any return before "[2/3] Recording VM
	// state" below completes -- a failed prepare, a declined sudo prompt --
	// so a VM that never got as far as a real record does not read as live
	// forever after (StarterPID is this test/CLI process's own, which in a
	// long-lived process never exits on its own). Once recorded is true
	// this is a no-op: state.UpsertVM below has written a real record --
	// disk resolved, network prepared, QEMU command built -- and it stays
	// even if QEMU itself then fails to launch, exactly as a single-VM host
	// always kept it.
	recorded := false
	defer func() {
		if recorded {
			return
		}
		_ = store.Update(func(s *state.State) error {
			if v := state.FindVM(s, vmName); v != nil && v.PID == 0 && v.StarterPID == os.Getpid() {
				state.RemoveVM(s, vmName)
			}
			return nil
		})
	}()

	writeLine(stdout, "[1/3] Preparing networking")
	if *network == "bridged" && runtime.GOOS == "darwin" {
		// vmnet happily builds a bridge onto an interface with no link. The
		// VM then boots, looks healthy, and never gets a lease, so refuse
		// here instead (kairos-io/kairos#4431).
		if err := vm.ValidateBridgeIface(networkIface); err != nil {
			return err
		}
		if vm.IsWiFiIface(networkIface) {
			writeLine(stdout, vm.WiFiBridgeWarning(networkIface))
		}
	}
	if *network == "shared" && runtime.GOOS == "linux" {
		// No uplink is named in this prompt and none is recorded on the
		// state, because shared enslaves no physical interface: the bridge
		// vm.PrepareLinuxShared builds carries ipv4.method shared and has the
		// tap as its only port, and that call clears
		// st.Network.BridgeInterface for the same reason. A prompt naming an
		// interface would be asking the user to consent to something this
		// mode never does, and `status` would then report a stale uplink as
		// this VM's.
		//
		// The parenthesis is a promise about what the next step does, and
		// vm.PrepareLinuxShared is what keeps it: it reads the bridge's port
		// list out of the kernel once the bridge is up and again once the tap
		// is on it, and before either of those as well when a device of the
		// bridge's name is already on this host -- which a clean first run
		// has not got, since both connections are written with autoconnect no
		// and nothing of that name exists until the first activation. It
		// refuses the run if the list holds anything it did not expect, and
		// attempts to take the bridge back down and delete the connections it
		// had just written rather than leave a host NIC on a NAT bridge; the
		// error it returns says what of that attempt failed. Until the tap is
		// activated it expects NO ports at all, so no name out of state.json
		// is exempt from the check.
		//
		// Stated no wider than that code states it. What is covered: any
		// port the kernel reports, whatever profile attached it and whether
		// the pre-flight's connection probes could see that profile at all;
		// a port list that could not be read, which is a refusal there and
		// not a pass; and the /sys stat that decides whether the check before
		// the first activation has anything to read at all, where only a
		// definite "no such file or directory" skips it and every other way
		// that stat can fail is a refusal too. What is not: an interface
		// attached in the instant after the last check returns, and any
		// attached later, since nothing reads the list again once the VM is
		// running.
		ok, err := confirm(stdin, stdout, *autoYes, "shared networking needs sudo to prepare a NAT bridge/tap (no uplink interface is used)")
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sudo permission denied")
		}
		if err := prepareLinuxShared(st, runtimeDir, vmIndex, siblingLive); err != nil {
			return err
		}
	}
	if *network == "bridged" && runtime.GOOS == "linux" {
		// networkIface is resolved by now: resolveBridgeUplink ran against
		// the mode this run settled on, and returned an error rather than an
		// empty string if the host offered no candidate. So this prompt names
		// the interface that is about to be enslaved, which is the only form
		// of it worth asking -- "(uplink: )" asks the user to agree to
		// whatever vm.PrepareLinuxBridge goes on to detect for itself.
		st.Network.BridgeInterface = networkIface
		ok, err := confirm(stdin, stdout, *autoYes, fmt.Sprintf("bridged networking needs sudo to prepare bridge/tap (uplink: %s)", st.Network.BridgeInterface))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("sudo permission denied")
		}
		if err := prepareLinuxBridge(st, runtimeDir, vmIndex, siblingLive); err != nil {
			return err
		}
		// Read back what the prepare enslaved instead of trusting what it
		// was asked for. From this caller that is a no-op today, and the
		// comment used to claim otherwise: the guarantee at the top of this
		// branch is that networkIface is non-empty here, and
		// vm.PrepareLinuxBridge runs a detection path of its own only when
		// the field arrives EMPTY -- so the name it writes back is the name
		// it was handed. The empty case is not live from here, and saying it
		// was made two comments eight lines apart contradict each other.
		//
		// The line stays as defence in depth, because what it keeps true is
		// a three-way agreement -- the prompt above, the recorded state and
		// the command line all naming one interface -- and it is the prepare
		// that decides which interface was enslaved, not this function.
		// Relax the guarantee above, or give the prepare any other reason to
		// use a different interface, and this readback is what stops the
		// three drifting apart without anyone noticing.
		networkIface = st.Network.BridgeInterface
	}

	// The guest NIC address is per-disk and sticky. A disk that already
	// carries one keeps it; one recorded before the field existed -- or one
	// created a moment ago -- gets a derived address filled in here and
	// persisted below. vm.MACForDisk hashes the disk name, so the address is
	// the same on every restart, which is what keeps a DHCP lease
	// attributable to this VM rather than to whichever guest last took
	// QEMU's single default address.
	//
	// A stored value is deliberately not validated here. netDeviceArg rejects
	// a corrupt one by name, which is an error the user can act on; quietly
	// replacing it with a derived address would start the VM on an address
	// state.json does not record.
	//
	// "Blank" has to mean the same thing here as it does there, which is why
	// this trims. netDeviceArg treats a whitespace-only value as unset and
	// emits a bare device, so a stored "   " that got past an untrimmed check
	// here handed the guest QEMU's single default address -- the collision
	// the derived one exists to avoid -- and then persisted the whitespace
	// below, so the next start did it again.
	//
	// disk.Name and not vmConfig.DiskName: the disk was re-fetched from state
	// after the review, so this is the name the image was created under. The
	// two differ whenever the review renamed a new disk, and deriving from
	// the pre-rename name would give the VM an address that no longer belongs
	// to the disk it boots.
	macAddress := disk.MAC
	if strings.TrimSpace(macAddress) == "" {
		macAddress = vm.MACForDisk(disk.Name)
	}

	// Use short names for socket (Unix socket path limit is ~108 chars), and
	// index 0's paths are byte-identical to a pre-multi-VM host; every index
	// above it gets its own socket and log so two VMs never share either.
	qgaSock, logPath := runtimePathsForIndex(runtimeDir, vmIndex)
	sshPort, httpPort, userModeHostBind := userModePortsForIndex(vmIndex)
	binary, qemuArgs, err := buildQEMUCommand(vm.StartConfig{
		ISOPath:          isoLocal,
		DiskPath:         disk.Path,
		QGASocketPath:    qgaSock,
		CPUs:             *cpus,
		MemoryMB:         *memory * 1024,
		NetworkMode:      *network,
		DisplayMode:      *display,
		BridgeIface:      bridgeInterfaceForMode(*network, networkIface),
		LinuxTapName:     st.Network.TapName,
		MACAddress:       macAddress,
		BiosPath:         biosPath,
		SSHPort:          sshPort,
		HTTPPort:         httpPort,
		UserModeHostBind: userModeHostBind,
	})
	if err != nil {
		return err
	}

	cmdName := binary
	cmdArgs := qemuArgs
	if vmnetNeedsSudo(runtime.GOOS, *network) {
		ok, err := confirm(stdin, stdout, *autoYes, vmnetSudoPrompt(*network))
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
	// Every write from here on goes through store.Update rather than the
	// plain store.Save this replaced, and touches only vmName's own entry in
	// s.VMs (state.UpsertVM) plus the disk/managed-path bookkeeping that
	// names disk.Name specifically -- never another VM's record. Two
	// concurrent starts each hold their own in-memory st, loaded before
	// either reached this point, so a plain Save here would publish a stale
	// copy of whatever the other one had already written; Update reloads
	// fresh under the same lock reservedAt was written under, so this
	// merges instead of overwriting.
	//
	// The final Update also re-asserts on disk.Name: if this VM's reservation
	// somehow no longer names us (it always should, since nothing else can
	// complete a start under this name while a live reservation stands), the
	// error surfaces here rather than silently recording a PID nobody asked
	// for.
	st.Network.Mode = *network
	st.Network.BridgeInterface = bridgeInterfaceForMode(*network, networkIface)
	tapConnName := ""
	if st.Network.BridgeName != "" {
		tapConnName = vm.TapConnNameForIndex(st.Network.BridgeName, vmIndex)
	}
	vmRecord := state.VM{
		Name:        vmName,
		Index:       vmIndex,
		ISOLocal:    isoLocal,
		DiskPath:    disk.Path,
		DiskName:    disk.Name,
		LogPath:     logPath,
		QemuBinary:  cmdName,
		QemuArgs:    cmdArgs,
		StartedAt:   state.NowRFC3339(),
		RuntimeDir:  runtimeDir,
		QGASockPath: qgaSock,
		TapName:     st.Network.TapName,
		TapConnName: tapConnName,
		NetworkMode: *network,
		SSHPort:     sshPort,
		HTTPPort:    httpPort,
		StarterPID:  os.Getpid(),
		StartingAt:  reservedAt,
	}
	if err := store.Update(func(s *state.State) error {
		// Mutated on the in-memory st first, and then copied whole into s,
		// whether s already has a record of this name (an existing disk
		// this run started) or not (a new disk this run just created,
		// added to st but never yet saved to disk).
		if srcDisk := state.FindDiskByName(st, disk.Name); srcDisk != nil {
			if vmConfig.MemoryGB > 0 {
				srcDisk.MemoryGB = vmConfig.MemoryGB
			}
			if vmConfig.CPUs > 0 {
				srcDisk.CPUs = vmConfig.CPUs
			}
			srcDisk.MAC = macAddress
			if existing := state.FindDiskByName(s, disk.Name); existing != nil {
				*existing = *srcDisk
			} else {
				state.AddDisk(s, *srcDisk)
			}
		}
		s.Network = st.Network
		state.AddManagedFile(s, logPath)
		state.AddManagedFile(s, qgaSock)
		state.UpsertVM(s, vmRecord)
		return nil
	}); err != nil {
		return err
	}
	// From here on vmRecord is a real record -- config fully resolved,
	// disk materialized, QEMU command built -- and worth keeping even if
	// QEMU itself then fails to launch (below) exactly as it was before
	// multi-VM support. Only the bare reservation stub from before this
	// point is what the deferred release above exists to clean up.
	recorded = true

	writeLine(stdout, "[3/3] Starting VM")
	writef(stdout, "Running: %s\n", renderCommand(cmdName, cmdArgs))
	if *network == "user" {
		// Said now rather than polled for. vm.IPLookup.Resolve documents
		// why: user mode is QEMU's own SLIRP stack, so its DHCP server runs
		// inside the QEMU process and writes no lease file the host can
		// read, and no frame from the guest reaches the host's neighbour
		// table, so no ARP entry exists either. Both host sources are out,
		// and the third -- the guest agent, if the image ships one --
		// reports the guest's own view of itself, 10.0.2.15 behind the NAT,
		// which is not an address anything on this host can connect to. The
		// way in is the forwarded ports named here.
		writeLine(stdout, userModeBlock())
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer func() {
		_ = logFile.Close()
	}()

	// While the VM runs, two things write to these streams at once: os/exec's
	// copier goroutines, carrying the guest's console, and the address poller
	// started below. io.Writer promises nothing about concurrent use -- an
	// *os.File survives it, the buffers the tests pass do not -- so both go
	// through one lock.
	//
	// The lock orders whole Write calls and nothing more. A block is written
	// with one of them, so it arrives whole; the console is copied in
	// whatever chunks the pipe delivers, so a block can still land between
	// two of those and split a console line in half.
	vmOut := &syncWriter{w: stdout}
	vmErr := &syncWriter{w: stderr}

	command := exec.Command(cmdName, cmdArgs...)
	if sf, ok := stdin.(*os.File); ok {
		command.Stdin = sf
	} else {
		command.Stdin = os.Stdin
	}
	// Which of the two the child writes through is childStdio's decision, and
	// it is a decision about the object rather than about the mode: the sudo
	// branch it exists for is only safe for a stream that is an *os.File.
	command.Stdout = childStdio(cmdName == "sudo", stdout, io.MultiWriter(vmOut, logFile))
	command.Stderr = childStdio(cmdName == "sudo", stderr, io.MultiWriter(vmErr, logFile))
	if err := command.Start(); err != nil {
		vmRecord.LastError = err.Error()
		_ = store.Update(func(s *state.State) error {
			state.UpsertVM(s, vmRecord)
			return nil
		})
		return fmt.Errorf("start qemu: %w", err)
	}
	vmRecord.PID = command.Process.Pid
	// An Update that cannot complete here (a timed-out lock, most likely) is
	// logged and the run continues rather than treated as a start
	// failure: QEMU is already running, and killing it over a bookkeeping
	// write would orphan nothing while still losing the VM the user asked
	// for. The PID is recorded on the next successful write instead --
	// either the exit-time one below, or a `status`/`reset` that reconciles
	// it -- so the record catches up rather than staying wrong forever.
	if err := store.Update(func(s *state.State) error {
		state.UpsertVM(s, vmRecord)
		return nil
	}); err != nil {
		writef(stderr, "warning: the VM's PID was not recorded: %v\n", err)
	}

	// The guest has no address until it has booted and asked for one, and
	// command.Wait below blocks for the whole life of the VM -- so "after
	// boot" is a goroutine running BESIDE the VM and not code placed after
	// that call.
	//
	// The goroutine never touches st or vmRecord directly. It records what
	// it finds through its own store.Update, keyed on vmName, which is what
	// lets a `status` in another terminal see the address while the VM is
	// still running without that write ever touching another VM's entry.
	ipCtx, cancelIPPoll := context.WithCancel(context.Background())
	ipDone := make(chan struct{})
	ipResolved := make(chan vm.IPResult, 1)
	if *network == "user" {
		// No poller at all for the mode with no host source to poll; the
		// block a few lines up has already said where this VM is reached.
		close(ipDone)
	} else {
		poll := vmIPPoll{
			Name: vmName,
			Lookup: vm.IPLookup{
				MAC:           macAddress,
				Mode:          *network,
				LeaseFile:     st.Network.DHCPLeaseFile,
				BridgeName:    st.Network.BridgeName,
				QGASocketPath: qgaSock,
			},
			Uplink:   st.Network.BridgeInterface,
			Timeout:  vm.DefaultIPPollTimeout,
			Interval: vm.DefaultIPPollInterval,
			GOOS:     runtime.GOOS,
			Stdout:   vmOut,
			Stderr:   vmErr,
			Store:    store,
		}
		go func() {
			defer close(ipDone)
			if res, ok := poll.run(ipCtx); ok {
				ipResolved <- res
			}
		}()
	}

	waitErr := command.Wait()
	// QEMU has exited, so the poll is cancelled and joined before anything
	// else writes state. vm.IPLookup.Poll's doc calls this exact usage out --
	// "the caller cancels this the moment QEMU exits" -- and the join is what
	// keeps the single-writer property above true: a poller still running
	// here would write state.json after this function had finished with it.
	cancelIPPoll()
	<-ipDone
	select {
	case res := <-ipResolved:
		vmRecord.IPAddress = res.IP
	default:
	}
	vmRecord.PID = 0
	vmRecord.StoppedAt = state.NowRFC3339()
	if waitErr != nil {
		vmRecord.LastError = waitErr.Error()
		_ = finalizeVMRecord(store, st, vmRecord)
		return fmt.Errorf("vm exited with error: %w (log: %s)", waitErr, logPath)
	}
	vmRecord.LastError = ""
	if err := finalizeVMRecord(store, st, vmRecord); err != nil {
		return err
	}
	writeLine(stdout, "vm exited")
	return nil
}

// --- the address the guest ends up on --------------------------------------

// webUIPort is the port the Kairos WebUI is served on inside the guest, and
// userModeSSHPort is the host port user mode forwards to the guest's sshd.
//
// They are literals here because they are literals in internal/vm too: both
// builders there carry a hostfwd list written out by hand -- tcp::2222-:22
// and tcp::8080-:8080 -- and nothing in that package exports the numbers. A
// shared constant would have to be declared there, which this change does not
// touch, so what keeps the two in step instead is a test that builds a
// user-mode QEMU command and looks for these ports in it.
const (
	webUIPort       = "8080"
	userModeSSHPort = "2222"
)

// runtimePathsForIndex returns the QGA socket and log paths for a VM at
// index. Index 0 is "runtime/qemu.sock" and "runtime/qemu.log",
// byte-identical to a pre-multi-VM host, and index N>=1 is
// "runtime/qemu-N.sock" and "runtime/qemu-N.log", so two VMs in the same
// config dir never share either.
func runtimePathsForIndex(runtimeDir string, index int) (qgaSock, logPath string) {
	if index == 0 {
		return filepath.Join(runtimeDir, "qemu.sock"), filepath.Join(runtimeDir, "qemu.log")
	}
	suffix := fmt.Sprintf("-%d", index)
	return filepath.Join(runtimeDir, "qemu"+suffix+".sock"), filepath.Join(runtimeDir, "qemu"+suffix+".log")
}

// userModePortsForIndex returns the SSH/HTTP forwards and the host address
// they bind. Index 0 keeps today's fixed 2222/8080 bound to every host
// interface -- index 0 must not change what it already exposes -- and index
// N>=1 gets 2222+N/8080+N bound to 127.0.0.1 only, which is new surface with
// no compatibility constraint to keep.
func userModePortsForIndex(index int) (sshPort, httpPort int, hostBind string) {
	if index == 0 {
		return 2222, 8080, ""
	}
	return 2222 + index, 8080 + index, "127.0.0.1"
}

// vmUpBlock is what a start prints once the guest's address is known.
//
// The first three lines are the block the issue specifies, character for
// character, down to the column the values line up on.
//
// The fourth is the source, which internal/vm asks its caller to print:
// vm.IPResult's own doc says "192.168.64.12 (from the ARP cache)" and the
// same address from the DHCP lease "deserve different amounts of trust from a
// human". An ARP entry is evidence the host exchanged frames with that MAC
// and can be left over from a previous boot; a line in our own DHCP server's
// lease file is that server saying what it handed this guest.
//
// It renders the usable case only. An address in 169.254.0.0/16 arrives
// here as an ordinary answer -- vm's usableIPv4 lets link-local through on
// purpose, because "a 169.254 address is what a guest that failed to get a
// lease genuinely has, and reporting it is more use than reporting nothing"
// -- and three lines that read as success are the wrong frame for one.
// vmLinkLocalBlock below is what the poll prints instead.
//
// Both values go through planValue although neither can carry anything a
// terminal acts on today: the address has been through vm's usableIPv4, which
// is net.ParseIP plus To4, so it is a dotted quad, and the source is one of
// three constants in that package. It is the same reason renderCommand sends
// its words through -- this is the boundary where a value this package did
// not choose becomes a line on a terminal, and the guard lives at the
// boundary rather than in each call site's memory.
func vmUpBlock(res vm.IPResult) string {
	ip := planValue(res.IP)
	return strings.Join([]string{
		"VM is up.",
		"  WebUI:  http://" + ip + ":" + webUIPort,
		"  SSH:    ssh kairos@" + ip,
		"  Source: " + planValue(res.Source),
	}, "\n")
}

// userModeBlock is the same shape for the one mode that has no address to
// look up.
//
// It is printed before QEMU is started, so it says where the VM will be
// reached and never that the VM is up: nothing here has observed a booted
// guest. What it can say without observing anything is where SLIRP forwards
// to, because those ports are on the command line about to be run.
//
// The note is the limitation checkDarwinPrivilege already spells out where it
// offers this mode as the fallback for a host that cannot get root. It is
// repeated here because a user who chose user mode directly never sees that
// error, and the difference -- no address on your network, so no second VM
// can reach this one -- is the whole reason the other two modes exist.
func userModeBlock() string {
	return strings.Join([]string{
		"user mode: the guest sits behind QEMU's user-mode NAT.",
		"  WebUI:  http://localhost:" + webUIPort,
		"  SSH:    ssh -p " + userModeSSHPort + " kairos@localhost",
		"  Note:   those two forwarded ports are the only way in. The guest",
		"          has no address on your network, so this mode supports a",
		"          single VM and cannot form a cluster.",
	}, "\n")
}

// ipPollFacts is everything the diagnostic below is allowed to say: the mode
// the run settled on, the address it asked about, the two interface names the
// two modes are diagnosed by, the platform, and the budget that elapsed.
//
// GOOS is a field rather than runtime.GOOS read inside the renderer for the
// reason noBridgeUplinkError gives for taking one as a parameter: a
// GOOS-gated sentence is a sentence only one CI leg can pin.
type ipPollFacts struct {
	Mode    string
	MAC     string
	Bridge  string
	Uplink  string
	GOOS    string
	Timeout time.Duration
}

// vmIPTimeoutNotice is what a start prints when the poll spent its whole
// budget without an answer.
//
// Never exiting silently is the requirement: a user left with no address has
// a VM that is running and unreachable, and the most likely reason -- the
// guest never got a lease -- is invisible from the host. So the notice names
// the mode and the MAC it asked about (which is what a lease file or an ARP
// table has to be searched for by hand), how long it waited, what usually
// causes this, and where the recorded answer lives.
//
// It says the run has stopped looking because it has: the poll is over when
// this prints, so no later `status` will fill the address in by itself.
func vmIPTimeoutNotice(f ipPollFacts) string {
	lines := []string{
		fmt.Sprintf("No address for the VM after %s. This run has stopped looking.", f.Timeout),
		"  Mode:   " + planValue(f.Mode),
		"  MAC:    " + planValue(f.MAC),
		"  Cause:  the guest may still be booting, or it never got a lease.",
	}
	lines = append(lines, ipPollCheckLines(f)...)
	lines = append(lines, "  Then:   kairos-lab status, for what this run recorded.")
	return strings.Join(lines, "\n")
}

// ipPollCheckLines is the part of the notice that differs per mode, because
// the two modes fail in different places: shared depends on a bridge and a
// DHCP server this tool started, bridged on a physical link and a DHCP server
// on the LAN that it did not.
//
// The names are the ones state recorded for this run, and they go through
// planValue because that is where they came from -- state.json, a file the
// reset and cleanup plans already treat as untrusted text.
func ipPollCheckLines(f ipPollFacts) []string {
	switch f.Mode {
	case "shared":
		// The bridge name is empty on macOS, where vmnet builds the
		// interface inside QEMU and nothing records what it called it.
		bridge := "the NAT bridge"
		if f.Bridge != "" {
			bridge = "the bridge " + planValue(f.Bridge)
		}
		return []string{"  Check:  " + bridge + " is up and the DHCP server behind it is running."}
	case "bridged":
		iface := "the bridged interface"
		if f.Uplink != "" {
			iface = planValue(f.Uplink)
		}
		lines := []string{"  Check:  " + iface + " has a link and a DHCP server on that network answered."}
		if f.GOOS == "darwin" {
			// On macOS a bridged guest has no lease file this tool can read
			// -- /var/db/dhcpd_leases is vmnet SHARED's database, which is
			// why Resolve's mode gate keeps a bridged run out of it -- so the
			// ARP cache is the host source left to answer, and it holds an
			// entry only for a MAC this host has exchanged frames with.
			lines = append(lines,
				"          On macOS this is answered from the host ARP cache, which",
				"          holds no entry until this host and the guest have",
				"          exchanged frames.")
		}
		return lines
	default:
		// Not reachable from runStart: -network and the reviewer accept only
		// the three modes in networkModes, and user mode starts no poll. It
		// is here so the renderer is total rather than silently dropping the
		// one line that says what to look at.
		return []string{"  Check:  the guest reached the network and a DHCP server answered."}
	}
}

// vmLinkLocalBlock is what a start prints when the poll resolved an address
// and that address is in 169.254.0.0/16.
//
// This is the requirement's failure mode -- "never exit silently leaving the
// user with only a link-local address" -- arriving through the success path
// rather than the timeout one, so the answer has to arrive there too. The
// address is printed rather than withheld, because it is still what the host
// found for this guest and the user may want it; what changes is the frame
// around it, so that the two URLs are not read as somewhere to go.
//
// The advice is ipPollCheckLines, the same list the timeout notice gives.
// 169.254.0.0/16 is the range RFC 3927 has a host fall back to when DHCP
// does not answer it, which is the cause that notice already names, so the
// thing to look at -- the DHCP server for this mode -- is the same thing.
//
// There is no "Then: kairos-lab status" row as the timeout notice has. That
// row exists because a timed-out run has an answer on record the terminal
// never showed; here the address is on the screen, and the status row adds
// only the same caveat these lines already carry (see linkLocalAddressNote).
func vmLinkLocalBlock(res vm.IPResult, f ipPollFacts) string {
	ip := planValue(res.IP)
	lines := []string{
		"The VM is up, but the address found for it is link-local.",
		"  WebUI:  http://" + ip + ":" + webUIPort,
		"  SSH:    ssh kairos@" + ip,
		"  Source: " + planValue(res.Source),
		"  Cause:  169.254.0.0/16 is what a guest assigns itself when no DHCP",
		"          server answers it. It is not routed, so the two URLs above",
		"          will not reach the VM.",
	}
	return strings.Join(append(lines, ipPollCheckLines(f)...), "\n")
}

// isLinkLocalIPv4 answers whether an address this tool is about to show a
// user is one of the self-assigned ones.
//
// net.ParseIP answers a nil IP for anything that is not an address, and To4
// a nil IP for an address that is not IPv4. net.IP is a slice and both To4
// and IsLinkLocalUnicast read a nil receiver as a length, so the chain
// answers false for junk rather than panicking on it -- which the `status`
// caller needs, because state.json is a 0644 file any process running as
// the user can write and the field is whatever it found there. The poll's
// own values have been through vm's usableIPv4, which opens with the same
// two calls.
func isLinkLocalIPv4(ip string) bool {
	v4 := net.ParseIP(ip).To4()
	return v4 != nil && v4.IsLinkLocalUnicast()
}

// linkLocalAddressNote annotates the address row in `status` the way
// bridgeIfaceLinkNote annotates the bridge interface, and for the same
// reason: the row is a value with a consequence a user cannot see in it.
//
// A user reading the row has less context than one watching a start -- the
// block that explained the address has scrolled away, or belongs to a
// terminal they never had -- so the row carries the short form itself rather
// than leaving 169.254.x.x to look like any other address.
func linkLocalAddressNote(ip string) string {
	if !isLinkLocalIPv4(ip) {
		return ""
	}
	// No interpolation, so nothing here needs escaping: the caller prints
	// the address itself through emptyAsNone.
	return " (link-local - self-assigned because no DHCP server answered; not reachable)"
}

// pollVMIP is vm.IPLookup.Poll behind a package-level var, the seam idiom
// this file already uses for host-touching calls (see prepareLinuxBridge).
//
// The real Poll runs the three sources for real, once a second for 45
// seconds: a subprocess against the host's neighbour table and a read on a
// guest-agent socket. What a test driving it would observe is whatever the
// machine running the suite has in its ARP cache -- and an address against a
// 52:54:00 MAC really can be in there, since that is QEMU's own prefix -- so
// what the poller prints, records and stays silent about is pinned through
// this instead. Nothing in production assigns it; the tests restore it with
// t.Cleanup.
var pollVMIP = func(ctx context.Context, lookup vm.IPLookup, timeout, interval time.Duration) (vm.IPResult, bool) {
	return lookup.Poll(ctx, timeout, interval)
}

// vmIPPoll is the address lookup that runs beside a started VM: what to ask
// about, how long to ask for, where to print the answer and where to record
// it.
type vmIPPoll struct {
	// Name is the VM this poll's findings belong to; recordVMIP keys its
	// Update on it so the write never touches any other VM's entry.
	Name     string
	Lookup   vm.IPLookup
	Uplink   string
	Timeout  time.Duration
	Interval time.Duration
	GOOS     string
	Stdout   io.Writer
	Stderr   io.Writer
	Store    *state.Store
}

// run performs the poll and says what it found, or why it found nothing. A
// poll that finds nothing does not fail the start: the VM is running either
// way and this is diagnostic output.
//
// Which kind of "nothing" it was decides whether anything is printed at all.
// Poll answers a cancelled context and an elapsed budget with the same false,
// and the two mean opposite things to the user: a VM quit ten seconds in must
// not be reported as a network that never came up. So a cancelled context --
// which from runStart means QEMU has exited -- prints nothing.
//
// The budget is checked as well as the context, because Poll has a third way
// of returning false: a MAC it cannot parse, answered immediately. The notice
// says how long was spent looking, and after that false no time has been
// spent at all. runStart cannot reach it (vm.BuildQEMUCommand has already
// rejected an unparseable stored MAC by name, and a blank one was replaced by
// a derived address well above), but the gate is what makes the sentence true
// of every caller rather than of this one.
func (p vmIPPoll) run(ctx context.Context) (vm.IPResult, bool) {
	started := time.Now()
	res, ok := pollVMIP(ctx, p.Lookup, p.Timeout, p.Interval)
	if ok {
		// An address that was found is not the same as an address that
		// works. A guest whose DHCP request went unanswered gives itself a
		// 169.254 address, the host's ARP cache picks it up like any other,
		// and vm's usableIPv4 hands it back as an answer -- so the block
		// that says a VM is reachable is chosen here, not by the lookup.
		if isLinkLocalIPv4(res.IP) {
			writeLine(p.Stdout, vmLinkLocalBlock(res, p.facts()))
		} else {
			writeLine(p.Stdout, vmUpBlock(res))
		}
		// Printed first and recorded second, and the recording is the half
		// that lasts: -serial mon:stdio is on all four display branches in
		// internal/vm, so the block above goes to a terminal the guest's own
		// boot console is scrolling past on and can be gone before it is
		// read. state.json is the durable copy and `status` is where a user
		// reads it back. A link-local address is recorded like any other,
		// and `status` qualifies it there the same way this block does.
		//
		// The warning this failure prints is scoped to the window it is
		// certainly true in, and its second line claims an attempt rather
		// than an outcome, because recordVMIP fails in two ways that end
		// differently. Its LOAD fails on a state file this process cannot
		// parse, and the save runStart makes when the VM exits rebuilds that
		// file from the state it is holding rather than reading the one this
		// attempt choked on -- so the address does land. Its SAVE fails on a
		// filesystem that will not take the write: a full disk, a quota, a
		// read-only remount, a config dir the user may no longer create a
		// file in. The exit save is the same store.Save on the same store, so
		// it fails again for as long as that condition holds and nothing is
		// recorded at all. Even after a load failure the later write is only
		// an attempt -- the disk can fill between the two -- so "tries" is
		// what this code backs on every path it covers. What is lost in both
		// halves is a `status` run made WHILE the VM is up, the very window
		// this poll exists to serve.
		if err := recordVMIP(p.Store, p.Name, res.IP); err != nil {
			writef(p.Stderr, "warning: the VM address was not recorded, so `kairos-lab status` will not show it while this VM is running: %s\n         this run tries to write the address to the state file again when the VM exits.\n", planValue(err.Error()))
		}
		return res, true
	}
	if ctx.Err() != nil {
		return vm.IPResult{}, false
	}
	if p.Timeout > 0 && time.Since(started) >= p.Timeout {
		writeLine(p.Stdout, vmIPTimeoutNotice(p.facts()))
	}
	return vm.IPResult{}, false
}

// facts is what this poll knows about the run it belongs to, in the shape
// the two diagnostics take.
//
// Both of them are reached from run and both end in ipPollCheckLines, so
// building the set once is what keeps a user who got a self-assigned address
// and a user who got no address at all pointed at the same thing -- which is
// right, because neither guest has a lease.
func (p vmIPPoll) facts() ipPollFacts {
	return ipPollFacts{
		Mode:    p.Lookup.Mode,
		MAC:     p.Lookup.MAC,
		Bridge:  p.Lookup.BridgeName,
		Uplink:  p.Uplink,
		GOOS:    p.GOOS,
		Timeout: p.Timeout,
	}
}

// childStdio picks what one of the child process's output streams is
// attached to: the writer this process was handed, or the wrapped one that
// shares a lock with the address poller and copies to the run's log file.
//
// The direct stream is the sudo branch. sudo on macOS may fail with "unable
// to allocate pty" when stdio is proxied through pipes, so a run launched
// through it keeps the terminal attached to the child instead.
//
// What makes that branch safe is the type test and not the caller's habits,
// which is why the test is here rather than in a comment. os/exec hands an
// *os.File to the child as a descriptor and starts no copier goroutine for
// it, so the poller writing through the wrapper is the only Go-level writer
// of that stream. Hand it any OTHER io.Writer and os/exec spawns a goroutine
// that writes THAT object -- the unwrapped one -- while the poller writes the
// wrapper around it: two writers of one writer's state, sharing no lock. main
// passes os.Stdout, so the real caller still gets the direct stream; anything
// else gets the wrapped one, and the log file with it.
func childStdio(sudo bool, direct, wrapped io.Writer) io.Writer {
	if sudo {
		if _, ok := direct.(*os.File); ok {
			return direct
		}
	}
	return wrapped
}

// syncWriter serialises writes to one io.Writer, so that the guest's console
// and the address poller can share a stream without tearing each other's
// output or racing on the writer's own state.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// finalizeVMRecord writes vmRecord's final state (the exit-time write, both
// on a clean exit and on a wait error) through a merge-safe store.Update,
// falling back to a plain overwrite built from st when Update cannot even
// complete a Load.
//
// The fallback matters because runStart's own write here is the process's
// last act before it exits: a state.json this process cannot currently parse
// -- corrupted by another write landing at exactly the wrong moment, or by
// anything else -- must not mean the VM's exit goes unrecorded forever.
// Before store.Update existed, a bare store.Save(st) already rebuilt the file
// unconditionally in that situation, since Save never reads what is currently
// on disk; falling back to exactly that keeps the property. st is this run's
// own last-known-good copy of everything else in the file -- Setup, Disks,
// Network, the managed lists -- so the rebuild loses nothing this run itself
// did not already have.
func finalizeVMRecord(store *state.Store, st *state.State, vmRecord state.VM) error {
	err := store.Update(func(s *state.State) error {
		// Merge rather than overwrite: the IP poll's own Update (or a
		// `status`/`reset` from another terminal) may have touched this
		// record since vmRecord was last synced with disk, most notably the
		// IPAddress field, which the caller may hold a stale (empty) copy
		// of if the poll's own write landed after it was last read.
		if v := state.FindVM(s, vmRecord.Name); v != nil && vmRecord.IPAddress == "" {
			vmRecord.IPAddress = v.IPAddress
		}
		state.UpsertVM(s, vmRecord)
		return nil
	})
	if err == nil {
		return nil
	}
	state.UpsertVM(st, vmRecord)
	return store.Save(st)
}

// recordVMIP puts the address on name's own VM record through a Store.Update
// of its own.
//
// It deliberately does not take the caller's *state.State: runStart is
// blocked in command.Wait() with a copy of the state it loaded before the VM
// started, and saving that copy from here would undo every field written
// since -- by this same start's own later writes, and by any sibling VM's.
// Update reloads fresh and changes only name's entry, which is what makes
// the address visible to a `status` run from another terminal while this VM
// is still running without disturbing anything else in state.json.
func recordVMIP(store *state.Store, name, ip string) error {
	return store.Update(func(st *state.State) error {
		v := state.FindVM(st, name)
		if v == nil {
			return fmt.Errorf("vm %q no longer has a state record", name)
		}
		v.IPAddress = ip
		return nil
	})
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

	platformLabel := st.Platform.OS + "/" + st.Platform.Arch
	if st.Platform.OS == "" {
		platformLabel = p.OS + "/" + p.Arch + " (detected)"
	}
	pm := st.Platform.PackageManager
	if pm == "" {
		pm = p.PackageManager + " (detected)"
	}

	// Every row below carries a value out of state.json, which is a 0644
	// file any process running as the user can write, to a terminal -- the
	// same untrusted path the reset and cleanup plans take, and the reason
	// planValue exists. These rows used to print theirs with a bare %s. A
	// stored bridge interface of "eth0" followed by CSI 2K (erase this
	// line), CSI 1G (back to column one) and a newline printed a forged "vm
	// running: true" row and erased the row the payload had arrived on,
	// leaving the forgery standing above the real "vm running: false" with
	// nothing to show where it came from.
	//
	// So the guard is applied here the way it is in the plans: emptyAsNone
	// and joinOrNone escape what they return, and the rows that format a
	// value some other way call planValue themselves.
	writef(stdout, "platform: %s\n", planValue(platformLabel))
	writef(stdout, "package manager: %s\n", planValue(pm))
	writef(stdout, "dependencies present now: %s\n", joinOrNone(present))
	writef(stdout, "dependencies pre-existing: %s\n", joinOrNone(st.Setup.PreExistingDeps))
	writef(stdout, "dependencies installed by kairos-lab: %s\n", joinOrNone(st.Setup.InstalledByKairosLab))
	writef(stdout, "managed dirs: %s\n", joinOrNone(st.ManagedDirs))
	writef(stdout, "managed files: %s\n", joinOrNone(st.ManagedFiles))

	// One block per VM (M5), in the order state.json carries them -- disk
	// name, mode, tap, running, pid, address are all per-VM now, where a
	// single-VM host used to see them as top-level rows. iso source/path and
	// disk path move into the block for the same reason: they are per-VM
	// facts.
	if len(st.VMs) == 0 {
		writeLine(stdout, "vms: none")
	}
	for _, v := range st.VMs {
		running := vm.VMIsLive(v)
		name := v.Name
		if name == "" {
			name = "(unnamed)"
		}
		writef(stdout, "vm: %s\n", planValue(name))
		writef(stdout, "  iso source: %s\n", emptyAsNone(v.ISOSource))
		writef(stdout, "  iso path: %s\n", emptyAsNone(v.ISOLocal))
		writef(stdout, "  disk path: %s\n", emptyAsNone(v.DiskPath))
		writef(stdout, "  network mode: %s\n", emptyAsNone(v.NetworkMode))
		writef(stdout, "  vm ip address: %s%s\n", emptyAsNone(v.IPAddress), linkLocalAddressNote(v.IPAddress))
		if v.NetworkMode == "user" {
			sshPort, httpPort, _ := userModePortsForIndex(v.Index)
			writef(stdout, "  user mode forwards: ssh localhost:%d, http localhost:%d\n", sshPort, httpPort)
		}
		writef(stdout, "  vm running: %t\n", running)
		if running {
			writef(stdout, "  vm pid: %d\n", v.PID)
		}
		if v.LastError != "" {
			writef(stdout, "  last vm error: %s\n", planValue(v.LastError))
		}
	}

	// The uplink stays a bridged-only row: shared clears the field on
	// purpose, because the bridge it builds has the tap as its only port and
	// attaches to no host interface.
	if st.Network.Mode == "bridged" {
		writef(stdout, "bridge iface: %s%s\n", emptyAsNone(st.Network.BridgeInterface), bridgeIfaceLinkNote(st.Network.BridgeInterface))
	}
	// The bridge and the taps are not bridged's alone: on Linux
	// vm.PrepareLinuxShared records BridgeName exactly as the bridged path
	// does, and it is what a user needs to name when a shared VM cannot be
	// reached -- which is the default mode. This row is rebuilt from the VM
	// entries rather than from a single st.Network.TapName, which multi-VM
	// support stops writing as a trusted value (M1/M2): every live VM may
	// have its own tap on the one shared bridge.
	//
	// That is a LINUX sentence, and the row is gated on BridgeName for
	// exactly that reason: PrepareLinuxBridge and PrepareLinuxShared are its
	// only writers. macOS hands the bridging to QEMU's vmnet backend and
	// records no bridge name in any mode.
	if st.Network.Mode != "user" && st.Network.BridgeName != "" {
		var taps []string
		for _, v := range st.VMs {
			if v.TapName != "" {
				taps = append(taps, v.TapName)
			}
		}
		writef(stdout, "bridge resources: bridge=%s taps=%s\n", emptyAsNone(st.Network.BridgeName), joinOrNone(taps))
	}

	// Quarantined records (M1): named rather than silently dropped, so a
	// state.json a future or buggy binary corrupted is visible here instead
	// of just missing a VM the user knows they started.
	for _, q := range st.Quarantined {
		writef(stdout, "quarantined vm record: %s (%s)\n", planValue(q.Name), planValue(q.Reason))
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
	if err := rejectPositionalArgs(fs, "remove a single disk with -disk"); err != nil {
		return err
	}

	st, err := store.Load()
	if err != nil {
		return err
	}
	if err := requireSetup(st); err != nil {
		return err
	}

	// The refusal is per-VM (M5): with -disk X, only X's own VM being live
	// blocks the reset; without it, every live VM blocks it, and each is
	// named so the user knows which to exit first.
	if *diskToRemove != "" {
		if v := state.FindVM(st, *diskToRemove); v != nil && vm.VMIsLive(*v) {
			return fmt.Errorf("vm %q is still running (PID %d). Exit it first (Ctrl-a x in serial console)", *diskToRemove, v.PID)
		}
	} else if names := liveVMNames(st); len(names) > 0 {
		return fmt.Errorf("vm(s) still running: %s. Exit them first (Ctrl-a x in serial console)", strings.Join(names, ", "))
	}

	// Collect paths to remove
	var paths []string
	var disksToRemove []state.Disk
	var vmsToRemove []state.VM

	if *diskToRemove != "" {
		// Remove specific disk
		disk := state.FindDiskByName(st, *diskToRemove)
		if disk == nil {
			return fmt.Errorf("disk not found: %s", *diskToRemove)
		}
		disksToRemove = append(disksToRemove, *disk)
		paths = append(paths, disk.Path)
		if v := state.FindVM(st, *diskToRemove); v != nil {
			vmsToRemove = append(vmsToRemove, *v)
		}
	} else {
		// Remove all disks
		for _, d := range st.Disks {
			disksToRemove = append(disksToRemove, d)
			paths = append(paths, d.Path)
		}
		vmsToRemove = append(vmsToRemove, st.VMs...)
	}

	// Add each targeted VM's own runtime files.
	for _, v := range vmsToRemove {
		paths = append(paths, v.LogPath, v.QGASockPath)
	}

	// Whether any VM NOT being removed is still live, which decides whether
	// the shared bridge itself may come down once every targeted VM's own
	// tap is gone -- every VM in this config dir shares one bridge, each with
	// its own tap on it, so the bridge can only go once no sibling's tap
	// still needs it.
	removing := make(map[string]bool, len(vmsToRemove))
	for _, v := range vmsToRemove {
		removing[v.Name] = true
	}
	siblingLive := false
	for _, v := range st.VMs {
		if !removing[v.Name] && vm.VMIsLive(v) {
			siblingLive = true
		}
	}

	toRemove, toSkip := splitRemovalPaths(paths, st)

	writeLine(stdout, "reset plan:")
	if len(disksToRemove) > 0 {
		writeLine(stdout, "Will remove disks:")
		for _, d := range disksToRemove {
			writef(stdout, "  - %s\n", planValue(d.Name))
		}
	}
	printRemovalPlan(stdout, "reset", toRemove, toSkip)
	hasStaleNetwork := vm.HasStaleNetworkResources(st)
	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		// Both names come out of state.json, which anything running as the
		// user can write, and these rows reach the terminal just above the
		// confirmation prompt -- a stored newline plus a CSI sequence would
		// forge a plan row and erase the real one, so the user would consent
		// to a plan they were never shown. The validator in internal/vm only
		// fires later, inside the cleanup itself. Nothing is escaped here:
		// printList runs every row through planValue, which quotes a row only
		// when it carries something a terminal would act on, so an ordinary
		// run still reads "bridge: kairoslab0". Escaping at this call site
		// would protect these two rows and no others -- which is exactly the
		// hole this replaced.
		rows := []string{"bridge: " + nonEmpty(st.Network.BridgeName, vm.DefaultBridgeName)}
		if len(vmsToRemove) == 0 {
			// No VM record to key off, which is what a fresh single-VM host
			// (never started, or already reset) looks like; the default tap
			// name is what a plain teardown has always shown here.
			rows = append(rows, "tap: "+vm.DefaultTapName)
		}
		for _, v := range vmsToRemove {
			rows = append(rows, "tap: "+nonEmpty(v.TapName, vm.DefaultTapName))
		}
		if siblingLive {
			rows = append(rows, "(the bridge itself is left standing: another VM is still using it)")
		}
		printList(stdout, "Will clean up network resources", rows)
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
		// planValue on both halves: the echo and the error that may follow it
		// carry the same stored path, and the error is the one that can
		// rewrite the echo above it -- the only record of what was deleted.
		writef(stdout, "Removing: %s\n", planValue(p))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", planValue(p), bareFileError(err))
		}
		state.RemoveManagedFile(st, p)
	}

	// Remove disks from state
	for _, d := range disksToRemove {
		state.RemoveDisk(st, d.Name)
	}

	// The error itself is kept, not a bool: it is the only thing that knows
	// WHICH part of the teardown failed. A refusal means nothing was touched
	// and a stored name has to be corrected; a partial failure means some
	// connections and links went and others did not, with the stored names
	// perfectly fine. The outcome reported at the end says which of those
	// happened by carrying this error into it.
	var networkCleanupErr error
	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		writeLine(stdout, "Cleaning up the network kairos-lab created...")
		// Sibling-aware, and ordered so the shared bridge comes down at most
		// once (M3): every target but the last is cleaned up as though a
		// sibling is live -- only its own tap goes -- and the last one
		// actually removes the bridge, unless siblingLive says a VM outside
		// this reset is still using it, in which case none of them do.
		for i, v := range vmsToRemove {
			last := i == len(vmsToRemove)-1
			if err := vm.CleanupLinuxBridge(st, v, siblingLive || !last); err != nil {
				writef(stdout, "warning: bridge cleanup failed for vm %q: %v\n", v.Name, err)
				networkCleanupErr = err
			}
		}
	} else if hasStaleNetwork {
		writeLine(stdout, "Cleaning up stale network resources...")
		if err := vm.CleanupStaleNetworkResources(st); err != nil {
			writef(stdout, "warning: stale network cleanup failed: %v\n", err)
			networkCleanupErr = err
		}
	}

	for _, v := range vmsToRemove {
		state.RemoveVM(st, v.Name)
	}
	if err := store.Save(st); err != nil {
		return err
	}
	// The disks and files are gone either way, which is why the failure above
	// is a warning and not an abort. But "reset complete" after a teardown
	// that did not finish is a lie the user has no way to see through, and the
	// next reset would print the same warning forever.
	//
	// What did not happen is not guessed at: the network layer returns the
	// failures it collected, and this message carries them instead of
	// asserting that every resource is still on the host and that a stored
	// name is at fault. A refusal means exactly that; a partial teardown
	// means some resources went and the names were never the problem.
	if networkCleanupErr != nil {
		return fmt.Errorf("reset incomplete: disks and files were removed, but the network cleanup did not finish: %w. What it names is still on the host; if the failure is about a stored bridge or tap name, correct it in stored configuration (%s), then run reset again", networkCleanupErr, store.StatePath)
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
	if err := rejectPositionalArgs(fs, ""); err != nil {
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
		// Both names come out of state.json, which anything running as the
		// user can write, and these rows reach the terminal just above the
		// confirmation prompt -- a stored newline plus a CSI sequence would
		// forge a plan row and erase the real one, so the user would consent
		// to a plan they were never shown. The validator in internal/vm only
		// fires later, inside the cleanup itself. Nothing is escaped here:
		// printList runs every row through planValue, which quotes a row only
		// when it carries something a terminal would act on, so an ordinary
		// run still reads "bridge: kairoslab0". Escaping at this call site
		// would protect these two rows and no others -- which is exactly the
		// hole this replaced.
		rows := []string{"bridge: " + nonEmpty(st.Network.BridgeName, vm.DefaultBridgeName)}
		if len(st.VMs) == 0 {
			rows = append(rows, "tap: "+vm.DefaultTapName)
		}
		for _, v := range st.VMs {
			rows = append(rows, "tap: "+nonEmpty(v.TapName, vm.DefaultTapName))
		}
		printList(stdout, "Will clean up network resources", rows)
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

	// cleanup removes everything unconditionally, so unlike reset's -disk
	// case every live VM blocks it, named so the user knows which to exit.
	if names := liveVMNames(st); len(names) > 0 {
		return fmt.Errorf("vm(s) still running: %s. Exit them first (Ctrl-a x in serial console)", strings.Join(names, ", "))
	}

	// The error itself is kept, not a bool: it is the only thing that knows
	// WHICH part of the teardown failed. A refusal means nothing was touched
	// and a stored name has to be corrected; a partial failure means some
	// connections and links went and others did not, with the stored names
	// perfectly fine. The outcome reported at the end says which of those
	// happened by carrying this error into it.
	var networkCleanupErr error
	if runtime.GOOS == "linux" && st.Network.CreatedByKairosLab {
		writeLine(stdout, "Cleaning up the network kairos-lab created...")
		if len(st.VMs) == 0 {
			// No VM record to key off -- a state reached by hand-editing or
			// by a sequence this binary does not otherwise produce. Fall
			// back to index 0, which is what a single-VM host always
			// cleaned up.
			if err := vm.CleanupLinuxBridge(st, state.VM{}, false); err != nil {
				writef(stdout, "warning: bridge cleanup failed: %v\n", err)
				networkCleanupErr = err
			}
		} else {
			// Every VM here is being removed, so only the last cleanup call
			// (siblingLive=false) actually takes the shared bridge down;
			// earlier ones remove just their own tap. See runReset for the
			// same pattern.
			for i, v := range st.VMs {
				last := i == len(st.VMs)-1
				if err := vm.CleanupLinuxBridge(st, v, !last); err != nil {
					writef(stdout, "warning: bridge cleanup failed for vm %q: %v\n", v.Name, err)
					networkCleanupErr = err
				}
			}
		}
	} else if hasStaleNetwork {
		writeLine(stdout, "Cleaning up stale network resources...")
		if err := vm.CleanupStaleNetworkResources(st); err != nil {
			writef(stdout, "warning: stale network cleanup failed: %v\n", err)
			networkCleanupErr = err
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
		writef(stdout, "Removing file: %s\n", planValue(p))
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove file %s: %w", planValue(p), bareFileError(err))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirsToRemove)))
	for _, d := range dirsToRemove {
		writef(stdout, "Removing directory: %s\n", planValue(d))
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("remove directory %s: %w", planValue(d), bareFileError(err))
		}
	}
	if err := store.RemoveStateFile(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Same reasoning as reset, minus the way out: the state file has just been
	// removed, so there is no stored name left to correct and no command left
	// to re-run. Whatever the teardown could not remove has to be removed by
	// hand, and saying "cleanup complete" here would be how the user never
	// learns that.
	if networkCleanupErr != nil {
		return fmt.Errorf("cleanup incomplete: files and dependencies were removed, but the network cleanup did not finish: %w. What it names is still on the host, and stored configuration is gone, so remove those connections and links with nmcli by hand", networkCleanupErr)
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
	writeLine(w, "  reset [-disk name]   Remove disks and network (keep setup/ISOs)")
	writeLine(w, "  cleanup              Remove everything created by tool")
	writeLine(w, "  version              Print CLI version")
	writeLine(w, "")
	// The flags listed here are the ones a first run has to decide: which
	// disk and ISO to boot, and the two settings whose default can be the
	// wrong one for the host in front of the user. -network, because shared
	// is the default that works on the most hosts rather than the one a user
	// putting the VM on the LAN wants, and -display, because window passes
	// QEMU -display default (see internal/vm) where a user on a remote
	// shell wants the serial console instead. The rest of what runStart
	// declares -- -disk-size, -memory, -cpus, -bridge-if, -yes -- tunes a
	// run whose shape is already settled, and `kairos-lab start -h` prints
	// every flag the start flag set declares for whoever needs one.
	//
	// The value column is padded to line the descriptions up with the ones
	// in the Commands block above, which leaves 19 columns for a flag and
	// its placeholder. "-network shared|bridged|user" does not fit in that,
	// so the modes are spelled out in the description instead of the value,
	// and the column stays where every other row in this usage has it.
	//
	// The two mode rows are built from the declarations rather than written
	// out beside them. Both used to be hand-copied, and a hand-copied row is
	// a row that goes stale silently: -network was missing from this block
	// for the whole life of the flag, and flipping the -display default from
	// window to serial left this row still saying window with nothing red.
	writeLine(w, "Start flags:")
	writeLine(w, "  -name <name>         Use/create disk with this name")
	writeLine(w, "  -new                 Create new disk (even if others exist)")
	writeLine(w, "  -no-iso              Boot without ISO (installed system)")
	writeLine(w, "  -iso <path>          Use specific ISO file")
	writef(w, "  -network <mode>      %s\n", modeUsageDescription("Network mode", networkModes, defaultNetworkMode))
	writef(w, "  -display <mode>      %s\n", modeUsageDescription("Display mode", displayModes, defaultDisplayMode))
	writeLine(w, "")
	writeLine(w, "Exit VM with Ctrl-a x (QEMU serial console quit)")
}

// modeUsageDescription renders the description half of one of the usage
// block's enumerated-mode rows: the set of modes the flag accepts and the one
// it falls back to, both read from the same declarations the flag and the
// validation read. The flag package prints the default in its own format for
// `start -h` ((default "shared")); this is the plainer form the usage block
// has always used.
func modeUsageDescription(label string, modes []string, defaultMode string) string {
	return label + ": " + strings.Join(modes, "|") + " (default " + defaultMode + ")"
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
	MemoryGB     int
	CPUs         int
	NetworkMode  string
	NetworkIface string
	Display      string
	DownloadsDir string
	IsNewDisk    bool
	TakenNames   map[string]struct{}
}

func reviewVMConfig(cfg *vmStartConfig, stdin io.Reader, stdout io.Writer) (*vmStartConfig, error) {
	// systemRAMGB never changes during a session — compute once outside the loop.
	ramGB := systemRAMGB()
	ramHint := ""
	if ramGB > 0 {
		ramHint = fmt.Sprintf("  (%d GB available)", ramGB)
	}

	for {
		writeLine(stdout, "")
		diskFreeGB := freeSpaceGB(filepath.Dir(cfg.DiskPath))
		diskHint := ""
		if diskFreeGB > 0 {
			diskHint = fmt.Sprintf("  (%d GB free)", diskFreeGB)
		}
		writeLine(stdout, "VM Configuration:")
		writef(stdout, "  1) Disk name:    %s\n", cfg.DiskName)
		writef(stdout, "  2) Disk path:    %s\n", cfg.DiskPath)
		writef(stdout, "  3) Disk size:    %d GB%s\n", parseSizeGB(cfg.DiskSize), diskHint)
		if cfg.ISOPath != "" {
			writef(stdout, "  4) ISO:          %s\n", filepath.Base(cfg.ISOPath))
		} else {
			writeLine(stdout, "  4) ISO:          (none - booting from disk)")
		}
		writef(stdout, "  5) Memory:       %d GB%s\n", cfg.MemoryGB, ramHint)
		writef(stdout, "  6) CPUs:         %d  (%d logical CPUs on host)\n", cfg.CPUs, runtime.NumCPU())
		writef(stdout, "  7) Network:      %s\n", cfg.NetworkMode)
		if bridgedIfaceSelectable(cfg.NetworkMode) {
			writef(stdout, "  8) Net interface: %s\n", cfg.NetworkIface)
		} else {
			writeLine(stdout, "  8) Net interface: (n/a)")
		}
		writef(stdout, "  9) Display:      %s\n", cfg.Display)
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
			if !cfg.IsNewDisk {
				writeLine(stdout, "(disk name cannot be changed for an existing disk)")
				break
			}
			val, err := prompt(stdin, stdout, "Enter disk name")
			if err != nil {
				return nil, err
			}
			if val != "" {
				if err := validDiskName(val); err != nil {
					writef(stdout, "Invalid name: %v\n", err)
					break
				}
				if _, taken := cfg.TakenNames[val]; taken {
					writef(stdout, "A disk named %q already exists, choose a different name.\n", val)
					break
				}
				cfg.DiskName = val
				cfg.DiskPath = filepath.Join(filepath.Dir(cfg.DiskPath), val+".qcow2")
			}
		case 2:
			if !cfg.IsNewDisk {
				writeLine(stdout, "(disk path cannot be changed for an existing disk)")
				break
			}
			writeLine(stdout, "(disk path is derived from the disk name — change the name to update the path)")
		case 3:
			if cfg.IsNewDisk {
				val, err := prompt(stdin, stdout, "Enter disk size in GB (e.g., 20, 60)")
				if err != nil {
					return nil, err
				}
				if val != "" {
					gb, err := strconv.Atoi(strings.TrimSpace(val))
					if err != nil || gb <= 0 {
						writeLine(stdout, "Invalid disk size, enter a number in GB (e.g., 20, 60)")
					} else {
						cfg.DiskSize = fmt.Sprintf("%dG", gb)
					}
				}
			} else {
				writeLine(stdout, "Warning: changing disk size will delete and recreate the disk image.")
				writeLine(stdout, "Any existing data on the disk will be lost.")
				ok, err := confirm(stdin, stdout, false, "I understand and want to change the disk size")
				if err != nil {
					return nil, err
				}
				if !ok {
					writeLine(stdout, "Cancelled.")
					break
				}
				val, err := prompt(stdin, stdout, "Enter new disk size in GB (e.g., 20, 60)")
				if err != nil {
					return nil, err
				}
				if val != "" {
					gb, err := strconv.Atoi(strings.TrimSpace(val))
					if err != nil || gb <= 0 {
						writeLine(stdout, "Invalid disk size, enter a number in GB (e.g., 20, 60)")
					} else {
						cfg.DiskSize = fmt.Sprintf("%dG", gb)
					}
				}
			}
		case 4:
			writeLine(stdout, "")
			selectedISO, err := iso.SelectOrDownloadISO(cfg.DownloadsDir, stdin, stdout)
			if err != nil {
				writef(stdout, "ISO selection failed: %v\n", err)
			} else {
				cfg.ISOPath = selectedISO
			}
		case 5:
			val, err := prompt(stdin, stdout, "Enter memory in GB (e.g., 4, 8)")
			if err != nil {
				return nil, err
			}
			if val != "" {
				gb, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil || gb <= 0 {
					writeLine(stdout, "Invalid memory value, enter a number in GB (e.g., 4, 8)")
				} else {
					cfg.MemoryGB = gb
				}
			}
		case 6:
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
		case 7:
			val, err := prompt(stdin, stdout, "Enter network mode (shared, bridged or user)")
			if err != nil {
				return nil, err
			}
			// An empty answer is "leave it alone" and stays silent: Enter
			// is the way out of this sub-prompt, not a mistake. Anything
			// else that is not a mode prints the rejection and falls
			// through to the menu, so the user answers again inside the
			// review instead of losing the rest of the configuration they
			// just edited to a hard error.
			switch {
			case networkModeValid(val):
				cfg.NetworkMode = val
				// A mode chosen here is a mode the run did not arrive in,
				// so entry 8 can have nothing to show: a start that came
				// in as shared or user never resolved an interface. Fill
				// one in now, so the row above reads as the interface the
				// run is going to enslave rather than as a blank the user
				// has to know to go and set.
				//
				// A host that offers none is not an error here. runStart
				// resolves again after the review and reports it there;
				// refusing inside the menu would throw away every other
				// edit made in it, and the row simply stays empty until
				// then.
				if iface, err := resolveBridgeUplink(cfg.NetworkMode, cfg.NetworkIface); err == nil {
					cfg.NetworkIface = iface
				}
			case val != "":
				writeLine(stdout, "Invalid network mode, use 'shared', 'bridged' or 'user'")
			}
		case 8:
			if bridgedIfaceSelectable(cfg.NetworkMode) {
				candidates := bridgeIfaceCandidates()
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
				writeLine(stdout, "Invalid option (a network interface applies to bridged mode only, on Linux and macOS; shared mode attaches to no host interface)")
			}
		case 9:
			val, err := prompt(stdin, stdout, "Enter display mode (window or serial)")
			if err != nil {
				return nil, err
			}
			if displayModeValid(val) {
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

// systemRAMGB returns the host's total physical RAM in GB, or 0 if undetectable.
func systemRAMGB() int {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0
		}
		return int(b / (1024 * 1024 * 1024))
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					return 0
				}
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil {
					return 0
				}
				return int(kb / (1024 * 1024))
			}
		}
	}
	return 0
}

// freeSpaceGB returns the free disk space in GB at the given path, or 0 if undetectable.
func freeSpaceGB(path string) int {
	out, err := exec.Command("df", "-k", path).Output()
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0
	}
	kb, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return 0
	}
	return int(kb / (1024 * 1024))
}

// parseSizeGB parses a size string like "60G" or "60" into an integer GB value.
func parseSizeGB(s string) int {
	s = strings.TrimSuffix(strings.TrimSpace(strings.ToUpper(s)), "G")
	n, _ := strconv.Atoi(s)
	return n
}

// bridgeIfaceLinkNote annotates the bridge interface in `status` output with
// its link state, so a bridge onto a dead port is visible without the user
// having to reach for ifconfig (kairos-io/kairos#4431). It is empty on
// platforms that do not report one.
func bridgeIfaceLinkNote(iface string) string {
	if iface == "" {
		return ""
	}
	switch status := vm.BridgeIfaceStatus(iface); status {
	case "":
		return ""
	case "active":
		return " (link active)"
	default:
		// The interface name is not interpolated here -- only the status is,
		// and the caller has already escaped the name it prints beside this.
		// The status word is whatever followed "status:" in this host's
		// ifconfig output, so it is escaped for the same reason the rest of
		// the row is: it is text this package did not choose.
		return fmt.Sprintf(" (link %s - the VM will not get an address over this interface)", planValue(status))
	}
}

// bridgeIfaceCandidates lists the host interfaces bridged networking can use,
// most likely first. Linux bridges through a NetworkManager uplink, macOS
// through vmnet, so the two enumerate different things.
//
// It is a var rather than a func so a test can answer for the host. Both
// implementations shell out -- `ip route show default` on Linux, the vmnet
// interface list on macOS -- and the answer is whatever the machine running
// the suite happens to be plugged into: a container whose only default route
// is a filtered virtual device, or a macOS runner with no active link, both
// answer "nothing". Every assertion about which interface a bridged run names
// and records needs a known answer, and asking the host for one is how a test
// comes to pass on a laptop and fail on a CI leg.
var bridgeIfaceCandidates = func() []string {
	switch runtime.GOOS {
	case "linux":
		return vm.DetectUplinkCandidates()
	case "darwin":
		return vm.DetectBridgeIfaceCandidates()
	}
	return nil
}

// resolveBridgeUplink answers which host interface a bridged run attaches to,
// filling in a candidate from the host when the user named none.
//
// It is called twice by runStart, before and after the config review, and
// that is the point of it being a function: the mode is not final until the
// review returns, so a gate on the flag's mode and a gate on the reviewed one
// are two different gates. They used to be exactly that -- detection keyed on
// the flag, the sudo consent prompt keyed on the reviewed mode -- which
// agreed only for as long as the flag defaulted to bridged. Once the default
// moved to shared, a user who chose bridged at prompt 7 got a consent prompt
// naming no interface at all, and vm.PrepareLinuxBridge then picked one and
// enslaved it.
//
// Calling it a second time costs nothing when the first already answered: a
// non-empty interface is returned unchanged, and the host is asked only when
// there is nothing to return.
//
// The gate is bridgedIfaceSelectable, the same predicate the review's entry 8
// uses to decide whether an interface is the user's to choose. shared and
// user attach to no interface, so for them the flag's value is passed through
// untouched and dropped later by bridgeInterfaceForMode.
func resolveBridgeUplink(mode, iface string) (string, error) {
	if !bridgedIfaceSelectable(mode) || iface != "" {
		return iface, nil
	}
	candidates := bridgeIfaceCandidates()
	if len(candidates) == 0 {
		return "", noBridgeUplinkError(runtime.GOOS)
	}
	// With several candidates the first one wins and the config review lets
	// the user change it at entry 8.
	return candidates[0], nil
}

// noBridgeUplinkError is what a bridged run is told when the host offers no
// interface to attach to. The two platforms fail differently enough to be
// worth different sentences: on Linux there is no default route through
// anything physical, on macOS no interface reports an active link, and vmnet
// would happily bridge onto the dead one (kairos-io/kairos#4431).
//
// goos is a parameter rather than runtime.GOOS so that both messages are
// reachable from either CI leg. A GOOS-gated string is a string only one leg
// can pin, and this one tells the user their two ways out of the failure.
func noBridgeUplinkError(goos string) error {
	if goos == "darwin" {
		return fmt.Errorf("no host interface has a link, so bridged networking would leave the VM without an address (use -bridge-if to specify one, or -network user for port-forwarded access)")
	}
	return fmt.Errorf("no suitable uplink interface found for bridged networking (use -bridge-if to specify one, or -network user for port-forwarded access)")
}

// vmnetNeedsSudo reports whether QEMU itself has to be launched as root.
//
// Both vmnet modes, not just bridged: shared is -netdev vmnet-shared and
// needs root exactly as vmnet-bridged does (vm.RequireNetworkPrivilege's
// darwinRootModes is the same pair). Launched unprivileged, QEMU exits
// non-zero with a vmnet error and no VM. On Linux nothing here runs as root:
// the bridge and the tap are prepared beforehand by NetworkManager, and QEMU
// opens a tap that already belongs to the user.
//
// goos is a parameter and not runtime.GOOS because the decision is otherwise
// pinnable on one CI leg only -- narrowing it back to bridged alone, which is
// what it said before shared was wired up, survived the entire suite.
func vmnetNeedsSudo(goos, mode string) bool {
	return goos == "darwin" && (mode == "bridged" || mode == "shared")
}

// vmnetSudoPrompt is what that consent asks. It names the mode the user
// actually chose, so consent is asked for the run they asked for, and it is
// built here rather than inline for the same reason as above: the branch it
// is printed from runs on darwin only, so this is the only place a test on
// either leg can read it.
func vmnetSudoPrompt(mode string) string {
	return fmt.Sprintf("%s vmnet mode runs qemu with sudo", mode)
}

// networkModes is the whole set of modes the CLI accepts, and it lives here,
// alone, because the set used to be spelled out inline at each of the two
// places a mode string is checked -- once in runStart against the -network
// flag, and once in reviewVMConfig against what the user types at prompt 7 --
// and those two drifted the moment a mode was added. Adding a mode to the flag
// and forgetting the reviewer leaves the CLI in the state where a run can be
// started in the new mode but the config review cannot select it back, and
// rejects the very value the flag just handed it, which is invisible to anyone
// who passes -yes and unavoidable for everyone who does not.
//
// So a fourth mode is one entry in the slice below and nothing else: the
// flag's validation, the reviewer's prompt and both rejection messages then
// agree by construction.
//
// Every mode in this slice is runnable on both supported platforms. There
// used to be a second question here -- whether a mode the CLI accepts could
// actually be run on this host -- because nothing prepared the host side of
// shared on Linux; it was answered by a temporary networkModeUnavailable that
// refused shared at both places a mode is chosen. runStart now calls
// vm.PrepareLinuxShared, so the mode has a host side on Linux as well as on
// macOS and there is no such question left to ask. Which of these the
// -network flag defaults to is a separate decision, and it is
// defaultNetworkMode below.
//
// Matching is deliberately exact. "Shared", "SHARED" and " shared" are all
// rejected rather than folded, both because every other enumerated value in
// this CLI (the display mode validated right after the network one in
// runStart, the subcommand names in Run) is matched exactly too, and because
// a tolerated near-miss would be written to state.json and handed to
// internal/vm, where BuildQEMUCommand compares the mode exactly and quietly
// falls back to user networking for anything it does not recognise -- a VM
// that boots, looks healthy, and is on the wrong network.
var networkModes = []string{"shared", "bridged", "user"}

// defaultNetworkMode is what the -network flag falls back to, and it is
// shared because that is the mode that works on the most hosts with the
// fewest surprises: it needs no uplink interface, so it runs on a Wi-Fi-only
// laptop where bridged cannot (no guest frame leaves the host with a MAC the
// access point never saw associate), and unlike user mode it puts the guest
// on a NAT subnet of its own -- an address the host can route to, where a
// user-mode guest has none and is reached only on the two ports SLIRP
// forwards to localhost. bridged is still one flag away for anyone who needs
// the VM on the LAN itself.
//
// What is deliberately not claimed here is a cluster. That subnet could
// carry more than one guest, but nothing in this CLI asks for a second VM,
// and neither way around that is supported: runStart refuses outright while
// this config dir's VM is running, and a second run pointed at another
// config dir resolves the same bridge and tap names, since no flag sets
// them -- which on Linux is what the pre-flight in internal/vm calls stale
// and removes, taking the running VM's network with it.
const defaultNetworkMode = "shared"

// displayModes is the whole set of display modes the CLI accepts, kept the
// way networkModes is and for the reason that set gives: a mode spelled out
// inline at each place it is decided drifts from the copies of itself. The
// display mode had one at every such place -- the flag's default, the flag's
// usage string, the check in runStart, the reviewer's prompt 9 and the usage
// block printed with no arguments -- and nothing derived any of them from
// anything. Flipping the flag's declared default from window to serial left
// go build, go vet and the whole of go test ./... green while both usage
// listings still said window. (The reviewer's question and its rejection
// line name the modes too; those are sentences addressed to a user rather
// than a set the code matches against, and they stay written out.)
//
// The order is the order the user is shown: the flag's usage string and the
// usage block are both built from this slice, so "window|serial" is one
// decision rather than a listing to keep in step by hand.
//
// Matching is exact here too. internal/vm's two QEMU builders switch on the
// mode and return "invalid display mode" for anything that is neither of
// these two (bar the empty string, which they read as serial), so a folded
// near-miss like "Window" would not start a VM either way -- it would fail
// later, inside the command builder, instead of at the flag the user typed.
var displayModes = []string{"window", "serial"}

// defaultDisplayMode is what the -display flag falls back to. window is what
// internal/vm turns into QEMU's -display default, a graphical window for
// someone sitting at the machine; serial is the mode to pass over a remote
// shell, where there is no display to open one on.
const defaultDisplayMode = "window"

// displayModeValid reports whether mode is one the CLI accepts. As with
// networkModeValid the empty string is not one of them, and the reviewer
// relies on that: an empty answer at prompt 9 means "leave it alone", so it
// has to fail this check and then be filtered out ahead of the rejection
// message rather than being accepted as a mode.
func displayModeValid(mode string) bool {
	return slices.Contains(displayModes, mode)
}

// networkModeValid reports whether mode is one the CLI accepts. The empty
// string is not one of them, which the reviewer relies on: an empty answer at
// prompt 7 means "leave it alone", so it must fail this check and then be
// filtered out ahead of the rejection message rather than being accepted here.
func networkModeValid(mode string) bool {
	return slices.Contains(networkModes, mode)
}

// bridgeInterfaceForMode answers what host interface this run attaches to, and
// it exists because the answer has to be the same in the two places runStart
// records it: on st.Network, which is what `status` prints and what the
// teardown reads, and on vm.StartConfig.BridgeIface, whose own doc says the
// field "is meaningful only for bridged mode".
//
// Only bridged attaches to an interface. shared builds a NAT bridge with no
// port but the tap -- vm.PrepareLinuxShared clears st.Network.BridgeInterface
// on purpose, and macOS vmnet-shared takes no ifname at all -- and user mode
// needs none. So the flag's value is dropped for both rather than carried:
// `start -network shared -bridge-if eth0` otherwise records an uplink the run
// never used, which is a lie state.json keeps until the next bridged start.
//
// What it answers for bridged is only as good as what it is handed, and the
// caller is careful about that. On Linux, iface is what vm.PrepareLinuxBridge
// reported having enslaved, not what it was asked for: re-deriving the
// recorded value from the flag instead is how a run that auto-detected and
// enslaved a real NIC came to record an empty interface. On macOS nothing
// prepares a bridge -- vmnet does that inside QEMU -- so there iface is the
// interface the run resolved, which is the only answer there is.
func bridgeInterfaceForMode(mode, iface string) string {
	if mode == "bridged" {
		return iface
	}
	return ""
}

// bridgedIfaceSelectable reports whether the network interface is the user's
// to choose in this configuration.
func bridgedIfaceSelectable(networkMode string) bool {
	return networkMode == "bridged" && (runtime.GOOS == "linux" || runtime.GOOS == "darwin")
}

// defaultBridgeIface leaves the bridge interface unset on every platform. It
// used to answer "en0" on macOS, which bridged the VM onto the built-in
// Ethernet port even when that port had no cable in it (kairos-io/kairos#4431).
// The interface is resolved from the host's default route in runStart instead,
// where it can also be validated.
func defaultBridgeIface() string {
	return ""
}

// linuxARM64Firmware lists the EDK2 builds a Linux arm64 host may have
// installed. QEMU's virt machine carries no firmware of its own, so one of
// these has to exist or this resolver fails right here, before QEMU is ever
// invoked -- the failure is this function's own error, not "No machine
// specified" or any other message QEMU prints, because QEMU is never started
// when this returns one (kairos-io/kairos#4858).
//
// The order is not "most portable first", and no entry below is a combined
// code+vars image: EDK2's ArmVirtQemu does not build one. Its [FD.QEMU_EFI]
// holds a 4 KiB jump stub plus FVMAIN_COMPACT; the writable varstore is
// [FD.QEMU_VARS], a separate flash device every packager ships as its own file
// beside the code image (Alpine's QEMU_VARS.fd, Debian's and Arch's
// AAVMF_VARS.fd / QEMU_VARS.fd, openSUSE's aavmf-aarch64-vars.bin). Entries
// 1-4 and 6 are all code-only build output; none of them is ever the
// combined image. Where entry 6 duplicates another entry, that duplication is
// sometimes only padding -- Arch's entry 6 is a symlink to its own entry 3,
// and Debian's entry 6 is entry 2's image truncated to a larger pflash size --
// but not always: Fedora's edk2.spec symlinks entry 6 to a build produced
// under edk2-build.fedora's [build.armvirt.aa64.silent] section, which sets a
// different DEBUG_PRINT_ERROR_LEVEL from the [build.armvirt.aa64.verbose]
// section that produces entry 3, so on Fedora (and CentOS/RHEL, which inherit
// the same spec -- %files aarch64 carries no %if fedora guard) entry 6 is a
// different EDK2 build, not a resize of entry 3. code-vs-code+vars is still
// the one distinction that never happens; padding-vs-different-build is not
// pinned the same way across every distribution that ships entry 6.
//
// What the order actually buys is smaller: each distribution reaches its own
// -bios-intended image first -- Alpine at entry 1, Debian/Ubuntu at entry 2,
// Fedora/CentOS/RHEL/Arch at entry 3, openSUSE at entry 4. Entry 5 catches a
// host with no firmware PACKAGE at all: an upstream-built QEMU, or an Alpine
// host with qemu-system-aarch64 but no aavmf. Entry 6 is belt-and-braces on
// the distributions that ship it -- Alpine, Debian/Ubuntu, Fedora/CentOS/RHEL
// and Arch; openSUSE ships no /usr/share/AAVMF/ at all -- and unreachable in
// practice, because each of those already matched an earlier entry first.
// Position barely matters beyond that: every entry here boots under plain
// -bios regardless of where it sits in the list.
//
// Entry 1 is Alpine's alone, not Debian's, despite the path's name. Debian and
// Ubuntu's qemu-efi-aarch64 package puts QEMU_EFI.fd under
// /usr/share/qemu-efi-aarch64/ (entry 2) and installs only the split
// AAVMF_CODE.fd / AAVMF_VARS.fd pair under /usr/share/AAVMF/; Alpine's aavmf
// package is the one that puts QEMU_EFI.fd there.
//
// Entry 5 has no firmware package behind it on any distribution: QEMU's own
// pc-bios/meson.build installs edk2-aarch64-code.fd as part of the emulator
// package itself, not aavmf, qemu-efi-aarch64, edk2-aarch64 or
// qemu-uefi-aarch64, so wherever this exact file is on disk it got there
// through the emulator's own package rather than a firmware one. That is why
// it survives on Alpine, where qemu-system-aarch64 carries the file directly
// and nothing strips it, and why it is absent everywhere else, for two
// different reasons: Debian and openSUSE strip QEMU's bundled edk2-*.fd out
// of their own packaging, and Fedora and Arch do not ship it. Without this
// entry, a user running an upstream-built QEMU -- or an Alpine user who
// installed the emulator but never pulled in aavmf -- has working firmware
// sitting on disk and is told none exists. It is also the same file
// macOSFirmwarePath resolves below, just under the Linux path QEMU installs
// it to instead of a Homebrew prefix.
var linuxARM64Firmware = []string{
	"/usr/share/AAVMF/QEMU_EFI.fd",            // apk: aavmf
	"/usr/share/qemu-efi-aarch64/QEMU_EFI.fd", // apt: qemu-efi-aarch64
	"/usr/share/edk2/aarch64/QEMU_EFI.fd",     // dnf/pacman: edk2-aarch64
	"/usr/share/qemu/qemu-uefi-aarch64.bin",   // zypper: qemu-uefi-aarch64
	"/usr/share/qemu/edk2-aarch64-code.fd",    // qemu's own bundled blob
	"/usr/share/AAVMF/AAVMF_CODE.fd",          // belt-and-braces; unreachable in practice
}

// linuxARM64FirmwarePath answers with the UEFI image an arm64 guest boots
// from, the first of linuxARM64Firmware that exists.
//
// It is handed to QEMU with plain -bios, the same as macOSFirmwarePath's
// result is in buildMacOS: there is no pflash pair, so the UEFI variable store
// has no backing block device and lives in RAM for the life of the process --
// boot variables set during one run do not survive the next. Every entry above
// is a code-only image regardless of position: -bios never gets a combined
// code+vars build to begin with, because EDK2's ArmVirtQemu does not build one
// and no distribution listed above packages one either. Kairos boots
// regardless, because EDK2 falls back to the removable-media path,
// EFI/BOOT/BOOTAA64.EFI, whenever no boot variable names anything else --
// which is the only state a fresh, RAM-backed store ever has.
func linuxARM64FirmwarePath() (string, error) {
	if path := firstExistingFile(linuxARM64Firmware); path != "" {
		return path, nil
	}
	return "", fmt.Errorf(
		"arm64 UEFI firmware not found in any of: %s -- install it with your package manager (qemu-efi-aarch64, edk2-aarch64, aavmf or qemu-uefi-aarch64), or, on Alpine, install qemu-system-aarch64 itself, which carries this firmware without aavmf",
		strings.Join(linuxARM64Firmware, ", "),
	)
}

// firstExistingFile returns the first candidate that stats successfully and
// is not a directory, or "" if none do.
func firstExistingFile(candidates []string) string {
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
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

// firmwarePathFor takes goos and goarch as arguments, the same reason
// buildLinuxFor does: so the linux/arm64 branch can be exercised from an
// amd64 test host, not only from a real arm64 one. runStart calls this with
// runtime.GOOS and runtime.GOARCH; a test calls it with whatever pair it
// wants to check.
//
// This function is the fix for a regression that already happened once:
// runStart used to switch on runtime.GOOS/runtime.GOARCH inline, so the
// linux/arm64 case was invisible to the amd64 CI leg that runs every test in
// this package -- deleting that case entirely left the whole suite green.
// Routing the selection through this function is what lets a table test put
// the linux/arm64 case under test without needing an arm64 host to run on,
// so the same deletion now fails a test instead of passing unnoticed. It is
// not itself pure -- it calls linuxARM64FirmwarePath, which os.Stats absolute
// paths, and macOSFirmwarePath, which forks brew and stats what that prints
// -- so the same (goos, goarch) pair can answer differently on different
// hosts, unlike buildLinuxFor (internal/vm/vm.go), which internal/vm's own
// vm_test.go calls pure for exactly that property.
func firmwarePathFor(goos, goarch string) (string, error) {
	switch {
	case goos == "darwin":
		return macOSFirmwarePath()
	case goos == "linux" && goarch == "arm64":
		return linuxARM64FirmwarePath()
	default:
		return "", nil
	}
}

// renderCommand renders the command line a start is about to run, for the
// "Running:" line printed just above the launch.
//
// Its words are not all the tool's own. The disk path, the ISO path and the
// tap name come out of state.json, a 0644 file any process running as the
// user can write, so this line carries untrusted text to a terminal exactly
// as the reset and cleanup plans do -- and on macOS it is the line recording
// a command about to run as root. It used to quote on " \t\n\"" alone, which
// let an ESC or a CR through raw: a stored path of "/tmp/k.qcow2<CSI>2K<CR>"
// printed a line that erased and rewrote itself.
//
// So every word goes through planValue, the same boundary the plans use. It
// costs ordinary output nothing: planValue returns an all-printable value
// unchanged, and a qemu command line is paths, numbers and comma-separated
// option lists.
//
// The space rule is renderCommand's own and stays on top of that, because
// planValue does not quote a space and this line is a command: a word with a
// space in it has to look like one word. \t and \n are no longer named here
// only because planValue already quotes them.
func renderCommand(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, renderCommandWord(name))
	for _, a := range args {
		parts = append(parts, renderCommandWord(a))
	}
	return strings.Join(parts, " ")
}

func renderCommandWord(word string) string {
	if rendered := planValue(word); rendered != word {
		return rendered
	}
	if strings.ContainsAny(word, " \"") {
		return strconv.Quote(word)
	}
	return word
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

// planValue renders a value bound for a plan the user is about to consent to.
//
// reset and cleanup build their plan straight from state.json -- a 0644 file
// any process running as the user can write -- and print it directly above the
// confirmation prompt. Every value in it is therefore untrusted text about to
// be written to a terminal: a stored newline forges a plan row, and a CSI
// sequence after it erases the real row that follows, so the user answers "y"
// to a plan they were never shown. Nothing upstream can prevent this for the
// plan, because the plan is printed before anything validates -- the interface
// validator in internal/vm only fires later, inside the cleanup itself, and
// paths, disk names and dependency names have no validator at all.
//
// So the guard lives here, at the point a value becomes a terminal line, and
// not at the call sites: a call site has to remember, and the previous fix
// escaped two rows while the identical attack walked through their siblings in
// the same plan.
//
// A value whose runes are all printable is returned unchanged, so an ordinary
// plan still reads "bridge: kairoslab0" and "- /home/u/.cache/kairos-lab/vm"
// rather than being uniformly quoted into noise. Anything else is handed to
// strconv.Quote.
//
// strconv.Quote, and not a hand-rolled escaper, because it is already exactly
// this function: it escapes every rune unicode.IsPrint rejects, and IsPrint is
// false for the whole of Cc, Cf, Co, Zl and Zp. That covers C0, DEL, the C1
// block including 8-bit CSI U+009B and NEL U+0085, LS/PS U+2028/U+2029, the
// bidi overrides such as RLO U+202E, the zero-width formatters, private-use
// runes, and OSC-8 hyperlinks (which need an ESC or a C1 OSC to begin). It
// also escapes bytes that are not valid UTF-8 at all, including encoded
// surrogates, as \x escapes.
// bareFileError strips the path an *os.PathError carries, leaving the reason.
// The caller already prints that path once, through planValue; the copy inside
// the error is redundant, and because %w renders it verbatim it is the one
// that carries a stored newline or escape sequence to the terminal. Unwrapping
// to the syscall errno keeps errors.Is working -- os.ErrPermission and friends
// match the errno, not the wrapper.
func bareFileError(err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

func planValue(s string) string {
	// The range loop below decodes an invalid byte as utf8.RuneError, and
	// U+FFFD is printable -- so a raw 0x9b (8-bit CSI, invalid on its own in
	// UTF-8) would pass the loop untouched. Reject invalid encoding first;
	// strconv.Quote renders those bytes as \x escapes.
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		// The three whitespace controls are spelled out even though
		// unicode.IsPrint already rejects all three. They are the runes that
		// do the damage -- a newline is what makes a forged row a row -- and a
		// reader should not have to know the Cc table to see that they are
		// caught here.
		if !unicode.IsPrint(r) || r == '\n' || r == '\r' || r == '\t' {
			return strconv.Quote(s)
		}
	}
	return s
}

func printList(w io.Writer, title string, values []string) {
	writef(w, "- %s:\n", title)
	if len(values) == 0 {
		writeLine(w, "  - none")
		return
	}
	for _, v := range values {
		writef(w, "  - %s\n", planValue(v))
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
		// The reason is a constant from splitRemovalPaths today, but it goes
		// through planValue too: this is the primitive, and the next reason
		// added here should not have to be audited.
		writef(w, "  - %s (%s)\n", planValue(k), planValue(values[k]))
	}
}

// joinOrNone and emptyAsNone are `status`'s two row renderers, and they
// escape what they return.
//
// Every value they are handed comes out of state.json, which is the same
// untrusted 0644 file the reset and cleanup plans read, and `status` prints
// it to a terminal with no prompt in the way. Escaping inside them rather
// than at each call site is the rule planValue's own doc sets out: the last
// fix at this boundary escaped two rows and the same attack then walked
// through their siblings.
//
// planValue returns an all-printable value unchanged, so an ordinary status
// still reads "disk path: /home/u/.cache/kairos-lab/vm/k.qcow2".
func joinOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	escaped := make([]string, 0, len(values))
	for _, v := range values {
		escaped = append(escaped, planValue(v))
	}
	return strings.Join(escaped, ", ")
}

func emptyAsNone(v string) string {
	if v == "" {
		return "none"
	}
	return planValue(v)
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func diskNameSet(st *state.State) map[string]struct{} {
	m := make(map[string]struct{}, len(st.Disks))
	for _, d := range st.Disks {
		m[d.Name] = struct{}{}
	}
	return m
}

var errSetupRequired = errors.New("setup has not been completed. Please run 'kairos-lab setup' first")

func requireSetup(st *state.State) error {
	if !state.IsSetupComplete(st) {
		return errSetupRequired
	}
	return nil
}

// liveVMNames returns the name of every VM in st that vm.VMIsLive counts as
// live, in state.json order. reset and cleanup both use it to name every VM
// that has to be exited first, rather than refusing over just one of them
// with no hint there were others.
func liveVMNames(st *state.State) []string {
	var names []string
	for _, v := range st.VMs {
		if vm.VMIsLive(v) {
			names = append(names, v.Name)
		}
	}
	return names
}
