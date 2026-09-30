package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/kairos-io/kairos-lab/internal/auroraboot"
	"github.com/kairos-io/kairos-lab/internal/deps"
	"github.com/kairos-io/kairos-lab/internal/platform"
	"github.com/kairos-io/kairos-lab/internal/state"
)

// Swapped by tests: host platform detection and the package install, neither
// of which a test may run for real.
var (
	detectPlatform  = platform.Detect
	installPackages = deps.Install
)

// auroraBootPullSize is what setup tells the user before pulling the image.
const auroraBootPullSize = "about 2.2 GB"

// setupAuroraBoot provides the `auroraboot` command: it settles on a container
// runtime, installing one on Linux only when the machine has none, pulls the
// pinned image, and writes the shim. Every action it takes and may later have
// to undo is recorded in st and saved at once, so a failure part way leaves
// state that cleanup can act on.
//
// A step that cannot go on for a reason that is not an error, such as a
// declined prompt or an existing auroraboot of the user's own, prints why and
// returns nil: it must not fail the setup of everything else.
func setupAuroraBoot(stdin io.Reader, stdout io.Writer, autoYes bool, runtimeFlag string, p platform.Info, st *state.State, store *state.Store) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	shimDir := auroraboot.ShimDir(home)
	shimPath := filepath.Join(shimDir, auroraboot.ShimName)

	if why := foreignAuroraBoot(shimPath); why != "" {
		writef(stdout, "skipping the auroraboot command: %s\n", why)
		return nil
	}

	rt, err := chooseRuntime(stdin, stdout, autoYes, runtimeFlag, p, st, store)
	if err != nil || rt == "" {
		return err
	}
	st.AuroraBoot.Runtime = rt
	if !slices.Contains(st.Setup.InstalledByKairosLab, rt) {
		st.Setup.PreExistingDeps = mergeUnique(st.Setup.PreExistingDeps, []string{rt})
	}
	if err := store.Save(st); err != nil {
		return err
	}

	ok, err := ensureAuroraBootImage(stdin, stdout, autoYes, rt, st, store)
	if err != nil || !ok {
		return err
	}

	content, err := auroraboot.RenderShim(rt, auroraboot.ImageRef(), p.OS)
	if err != nil {
		return err
	}
	path, createdDir, err := auroraboot.InstallShim(shimDir, content)
	if errors.Is(err, auroraboot.ErrNotManaged) {
		writef(stdout, "skipping the auroraboot command: %v\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	st.AuroraBoot.ShimPath = path
	if createdDir {
		st.AuroraBoot.ShimDirCreated = shimDir
	}
	if err := store.Save(st); err != nil {
		return err
	}
	writef(stdout, "installed %s (runs %s on %s)\n", path, auroraboot.ImageRef(), rt)
	if !dirOnPath(shimDir) {
		writef(stdout, "note: %s is not on your PATH; add it with: export PATH=\"%s:$PATH\"\n", shimDir, shimDir)
	}
	writeLine(stdout, "try it: auroraboot build-iso --output ./build <image>")
	return nil
}

// foreignAuroraBoot returns why the shim must not be written, or "" when it
// may: an auroraboot on PATH, or a file at the shim's own path, that
// kairos-lab did not write is the user's and is left alone.
func foreignAuroraBoot(shimPath string) string {
	if found, err := exec.LookPath(auroraboot.ShimName); err == nil && !auroraboot.IsManagedShim(found) {
		return "auroraboot is already installed at " + found
	}
	if _, err := os.Lstat(shimPath); err == nil && !auroraboot.IsManagedShim(shimPath) {
		return shimPath + " already exists and was not written by kairos-lab"
	}
	return ""
}

// chooseRuntime returns the runtime the shim will use, or "" when the step
// should end without one. A runtime is installed only when the machine has
// none at all, and only on Linux; a runtime that is present but not working is
// never installed over.
func chooseRuntime(stdin io.Reader, stdout io.Writer, autoYes bool, runtimeFlag string, p platform.Info, st *state.State, store *state.Store) (string, error) {
	name, present, reason := auroraboot.DetectRuntime(runtimeFlag)
	if name != "" {
		return name, nil
	}
	if len(present) > 0 || (runtimeFlag != "" && !auroraboot.ValidRuntime(runtimeFlag)) {
		writef(stdout, "skipping the auroraboot command: %s\n", reason)
		return "", nil
	}
	if p.OS != "linux" {
		writeLine(stdout, "skipping the auroraboot command: no container runtime is installed")
		writeLine(stdout, "install Docker Desktop, Colima or podman, then run 'kairos-lab setup' again")
		return "", nil
	}
	if p.PackageManager == "" {
		writeLine(stdout, "skipping the auroraboot command: no container runtime is installed and no package manager was detected to install one")
		return "", nil
	}

	choice := runtimeFlag
	if choice == "" {
		choice = "docker"
	}
	dep := deps.Docker()
	if choice == "podman" {
		dep = deps.Podman()
	}
	ok, err := confirm(stdin, stdout, autoYes, fmt.Sprintf("no container runtime found; install %s now", choice))
	if err != nil {
		return "", err
	}
	if !ok {
		writeLine(stdout, "skipping the auroraboot command: no container runtime")
		return "", nil
	}
	pkgs, err := deps.InstallablePackages(p.PackageManager, []deps.Dependency{dep})
	if err != nil {
		return "", err
	}
	ok, err = confirm(stdin, stdout, autoYes, "this step needs sudo to install packages")
	if err != nil {
		return "", err
	}
	if !ok {
		writeLine(stdout, "skipping the auroraboot command: sudo permission denied")
		return "", nil
	}
	if err := installPackages(p.PackageManager, pkgs, true); err != nil {
		return "", err
	}
	st.Setup.InstalledByKairosLab = mergeUnique(st.Setup.InstalledByKairosLab, []string{dep.Name})
	if err := store.Save(st); err != nil {
		return "", err
	}

	name, _, reason = auroraboot.DetectRuntime(choice)
	if name != "" {
		return name, nil
	}
	writef(stdout, "%s was installed but is not usable yet: %s\n", choice, reason)
	if choice == "docker" {
		writeLine(stdout, "start it and let your user talk to it, then run 'kairos-lab setup' again:")
		writeLine(stdout, "  sudo systemctl enable --now docker")
		writeLine(stdout, "  sudo usermod -aG docker \"$USER\"   (then log out and back in)")
		writeLine(stdout, "kairos-lab does not change groups or services itself")
	}
	return "", nil
}

// ensureAuroraBootImage makes sure the pinned image is in the runtime's store.
// An image that was already there is recorded as pre-existing and never
// removed; one setup pulls is recorded before the pull starts and dropped
// again if the pull fails. It reports false when the step should end here.
func ensureAuroraBootImage(stdin io.Reader, stdout io.Writer, autoYes bool, rt string, st *state.State, store *state.Store) (bool, error) {
	ctx := context.Background()
	ref := auroraboot.ImageRef()
	exists, err := auroraboot.ImageExists(ctx, rt, ref)
	if err != nil {
		return false, err
	}
	if exists {
		if !slices.Contains(st.AuroraBoot.PulledImages, ref) {
			st.AuroraBoot.PreExistingImages = mergeUnique(st.AuroraBoot.PreExistingImages, []string{ref})
		}
		writef(stdout, "image %s is already present\n", ref)
		return true, store.Save(st)
	}
	ok, err := confirm(stdin, stdout, autoYes, fmt.Sprintf("pull %s now (%s)", ref, auroraBootPullSize))
	if err != nil {
		return false, err
	}
	if !ok {
		writeLine(stdout, "skipping the auroraboot command: image not pulled")
		return false, nil
	}
	state.AddPulledImage(st, ref)
	if err := store.Save(st); err != nil {
		return false, err
	}
	if err := auroraboot.PullImage(ctx, rt, ref, stdout); err != nil {
		state.RemovePulledImage(st, ref)
		if serr := store.Save(st); serr != nil {
			return false, errors.Join(err, serr)
		}
		return false, err
	}
	return true, nil
}

// dirOnPath reports whether dir is one of the directories in $PATH.
func dirOnPath(dir string) bool {
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d != "" && filepath.Clean(d) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}
