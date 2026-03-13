package deps

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"

	"github.com/kairos-io/kairos-lab/internal/platform"
)

type Dependency struct {
	Name            string
	Binaries        []string
	InstallPackages map[string][]string
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
		deps = append(deps, Dependency{
			Name:     "dnsmasq",
			Binaries: []string{"dnsmasq"},
			InstallPackages: map[string][]string{
				"apt":    {"dnsmasq"},
				"dnf":    {"dnsmasq"},
				"yum":    {"dnsmasq"},
				"zypper": {"dnsmasq"},
				"pacman": {"dnsmasq"},
				"apk":    {"dnsmasq"},
			},
		})
	}
	return deps
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
	binary := "qemu-system-x86_64"
	if runtime.GOARCH == "arm64" {
		binary = "qemu-system-aarch64"
	}
	return Dependency{
		Name:     "qemu",
		Binaries: []string{binary, "qemu-img"},
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
