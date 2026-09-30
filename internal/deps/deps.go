package deps

import (
	"fmt"
	"os/exec"
	"sort"

	"github.com/kairos-io/kairos-lab/internal/platform"
)

type Dependency struct {
	Name            string
	Binaries        []string
	InstallPackages map[string][]string
	// Cask marks a dependency whose brew package is a Homebrew cask, not a
	// formula. It changes only how brew is called; see BrewCask.
	Cask bool
}

// BrewCask is the pseudo package manager name Install and Uninstall accept for
// `brew install --cask` and `brew uninstall --cask`. It exists so the package
// list and the package manager travel together through the same calls a
// formula takes.
const BrewCask = "brew-cask"

// ManagerFor returns the package manager name to hand Install or Uninstall for
// this dependency: BrewCask for a cask under brew, pm otherwise.
func (d Dependency) ManagerFor(pm string) string {
	if pm == "brew" && d.Cask {
		return BrewCask
	}
	return pm
}

func Required(info platform.Info) []Dependency {
	deps := []Dependency{}
	deps = append(deps, qemuDependency(info))
	if info.OS == "linux" {
		deps = append(deps, Dependency{
			Name:     "iproute2",
			Binaries: []string{"ip"},
			InstallPackages: map[string][]string{
				"apt":    {"iproute2"},
				"dnf":    {"iproute"},
				"yum":    {"iproute"},
				"zypper": {"iproute2"},
				"pacman": {"iproute2"},
				"apk":    {"iproute2"},
			},
		})
	}
	return deps
}

// Docker and Podman are the container runtimes the auroraboot shim can run on.
// They are deliberately not part of Required: setup installs one only when the
// machine has no runtime at all, so they are asked for separately and recorded
// under the runtime's own name. On macOS brew installs docker as the
// docker-desktop cask (the old cask name "docker" no longer exists) and podman
// as a formula; neither needs sudo.
//
// dnf's moby-engine is Fedora's package; RHEL and its rebuilds have no docker
// package at all, and setup reports the failure rather than guess at another
// repository. Installing docker also leaves its service disabled and the user
// outside the docker group; setup tells the user, it does not change either.
func Docker() Dependency {
	return Dependency{
		Name:     "docker",
		Binaries: []string{"docker"},
		Cask:     true,
		InstallPackages: map[string][]string{
			"brew":   {"docker-desktop"},
			"apt":    {"docker.io"},
			"dnf":    {"moby-engine"},
			"yum":    {"docker"},
			"zypper": {"docker"},
			"pacman": {"docker"},
			"apk":    {"docker"},
		},
	}
}

// Podman is the daemonless alternative to Docker; see Docker.
func Podman() Dependency {
	return Dependency{
		Name:     "podman",
		Binaries: []string{"podman"},
		InstallPackages: map[string][]string{
			"brew":   {"podman"},
			"apt":    {"podman"},
			"dnf":    {"podman"},
			"yum":    {"podman"},
			"zypper": {"podman"},
			"pacman": {"podman"},
			"apk":    {"podman"},
		},
	}
}

func DetectPresent(dep Dependency) bool {
	for _, b := range dep.Binaries {
		if _, err := exec.LookPath(b); err != nil {
			return false
		}
	}
	return true
}

func Missing(required []Dependency) []Dependency {
	missing := make([]Dependency, 0)
	for _, dep := range required {
		if !DetectPresent(dep) {
			missing = append(missing, dep)
		}
	}
	return missing
}

func PresentNames(required []Dependency) []string {
	out := make([]string, 0)
	for _, dep := range required {
		if DetectPresent(dep) {
			out = append(out, dep.Name)
		}
	}
	sort.Strings(out)
	return out
}

func InstallablePackages(pm string, deps []Dependency) ([]string, error) {
	pkgs := []string{}
	for _, dep := range deps {
		p, ok := dep.InstallPackages[pm]
		if !ok {
			return nil, fmt.Errorf("dependency %s has no package mapping for %s", dep.Name, pm)
		}
		pkgs = append(pkgs, p...)
	}
	return dedupeSorted(pkgs), nil
}

func UninstallablePackages(pm string, names []string, required []Dependency) ([]string, error) {
	byName := map[string]Dependency{}
	for _, d := range required {
		byName[d.Name] = d
	}
	pkgs := []string{}
	for _, n := range names {
		d, ok := byName[n]
		if !ok {
			continue
		}
		mapped, ok := d.InstallPackages[pm]
		if !ok {
			return nil, fmt.Errorf("dependency %s has no package mapping for %s", d.Name, pm)
		}
		pkgs = append(pkgs, mapped...)
	}
	return dedupeSorted(pkgs), nil
}

func qemuDependency(info platform.Info) Dependency {
	if info.OS == "darwin" && info.Arch == "arm64" {
		return Dependency{
			Name:     "qemu",
			Binaries: []string{"qemu-system-aarch64", "qemu-img"},
			InstallPackages: map[string][]string{
				"brew": {"qemu"},
			},
		}
	}
	if info.Arch == "arm64" {
		// An arm64 host needs the arm64 emulator and the EDK2 firmware that
		// goes with it. Naming the x86 packages here installed a qemu that
		// DetectPresent could never find, so setup asked for the same
		// packages forever (kairos-io/kairos#4858).
		return Dependency{
			Name:     "qemu",
			Binaries: []string{"qemu-system-aarch64", "qemu-img"},
			InstallPackages: map[string][]string{
				"apt":    {"qemu-system-arm", "qemu-efi-aarch64", "qemu-utils"},
				"dnf":    {"qemu-system-aarch64", "edk2-aarch64", "qemu-img"},
				"yum":    {"qemu-kvm", "edk2-aarch64", "qemu-img"},
				"zypper": {"qemu-arm", "qemu-uefi-aarch64", "qemu-tools"},
				// qemu-img is a package of its own on Arch and nothing in
				// qemu-system-aarch64's dependency closure pulls it in. The
				// amd64 map below gets it for free because qemu-base depends
				// on it; naming the emulator directly, as arm64 must, does
				// not (kairos-io/kairos#5018).
				"pacman": {"qemu-system-aarch64", "edk2-aarch64", "qemu-img"},
				"apk":    {"qemu-system-aarch64", "aavmf", "qemu-img"},
				"brew":   {"qemu"},
			},
		}
	}
	return Dependency{
		Name:     "qemu",
		Binaries: []string{"qemu-system-x86_64", "qemu-img"},
		InstallPackages: map[string][]string{
			"apt":    {"qemu-system-x86", "qemu-utils"},
			"dnf":    {"qemu-system-x86", "qemu-img"},
			"yum":    {"qemu-kvm", "qemu-img"},
			"zypper": {"qemu-x86", "qemu-tools"},
			"pacman": {"qemu-base"},
			"apk":    {"qemu-system-x86_64", "qemu-img"},
			"brew":   {"qemu"},
		},
	}
}

func dedupeSorted(values []string) []string {
	set := map[string]struct{}{}
	for _, v := range values {
		if v == "" {
			continue
		}
		set[v] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
