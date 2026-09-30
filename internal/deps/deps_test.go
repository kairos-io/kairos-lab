package deps

import (
	"slices"
	"strings"
	"testing"

	"github.com/kairos-io/kairos-lab/internal/platform"
)

func TestInstallablePackages(t *testing.T) {
	required := []Dependency{
		{Name: "qemu", InstallPackages: map[string][]string{"apt": {"qemu-system-x86", "qemu-utils"}}},
		{Name: "dnsmasq", InstallPackages: map[string][]string{"apt": {"dnsmasq"}}},
	}
	pkgs, err := InstallablePackages("apt", required)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("unexpected package count: %d", len(pkgs))
	}
}

func TestUninstallablePackages(t *testing.T) {
	required := []Dependency{
		{Name: "qemu", InstallPackages: map[string][]string{"apt": {"qemu-system-x86", "qemu-utils"}}},
		{Name: "dnsmasq", InstallPackages: map[string][]string{"apt": {"dnsmasq"}}},
	}
	pkgs, err := UninstallablePackages("apt", []string{"qemu"}, required)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkgs) != 2 {
		t.Fatalf("unexpected package count: %d", len(pkgs))
	}
}

// setup used to offer the x86 emulator on an arm64 host, which installs fine
// and leaves DetectPresent still looking for qemu-system-aarch64. See
// kairos-io/kairos#4858.
func TestQemuPackagesMatchTheArchitectureBinary(t *testing.T) {
	cases := []struct {
		arch       string
		wantBinary string
		wantPkg    string
		rejectPkg  string
	}{
		{"arm64", "qemu-system-aarch64", "qemu-system-arm", "qemu-system-x86"},
		{"amd64", "qemu-system-x86_64", "qemu-system-x86", "qemu-system-arm"},
	}

	for _, tc := range cases {
		t.Run(tc.arch, func(t *testing.T) {
			dep := qemuDependency(platform.Info{OS: "linux", Arch: tc.arch})
			if dep.Binaries[0] != tc.wantBinary {
				t.Fatalf("got binary %q, want %q", dep.Binaries[0], tc.wantBinary)
			}
			pkgs, err := InstallablePackages("apt", []Dependency{dep})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(pkgs, tc.wantPkg) {
				t.Errorf("apt packages %v are missing %q", pkgs, tc.wantPkg)
			}
			if slices.Contains(pkgs, tc.rejectPkg) {
				t.Errorf("apt packages %v offer %q, which ships the wrong emulator", pkgs, tc.rejectPkg)
			}
		})
	}
}

// Every package manager has to answer for arm64, otherwise
// InstallablePackages errors out and setup cannot install anything at all.
func TestQemuARM64CoversEveryPackageManager(t *testing.T) {
	dep := qemuDependency(platform.Info{OS: "linux", Arch: "arm64"})
	for _, pm := range []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"} {
		pkgs, err := InstallablePackages(pm, []Dependency{dep})
		if err != nil {
			t.Fatalf("%s: %v", pm, err)
		}
		for _, pkg := range pkgs {
			if strings.Contains(pkg, "x86") {
				t.Errorf("%s offers %q on arm64", pm, pkg)
			}
		}
	}
}

// packageProviders names, per package manager, the packages that ship each
// binary qemuDependency declares. A mapping is only usable if it can satisfy
// EVERY binary DetectPresent will look for, not just the emulator: setup
// installs the package list and then re-checks the binaries, so one binary no
// package provides makes setup report the dependency missing forever, with
// re-running it no help. That is kairos-io/kairos#4858 for the emulator and
// kairos-io/kairos#5018 for qemu-img.
//
// The two existing tests above both stop at Binaries[0]. This one is the
// reason they are not enough.
var packageProviders = map[string]map[string][]string{
	"qemu-system-aarch64": {
		"apt":    {"qemu-system-arm"},
		"dnf":    {"qemu-system-aarch64"},
		"yum":    {"qemu-kvm"},
		"zypper": {"qemu-arm"},
		"pacman": {"qemu-system-aarch64", "qemu-full"},
		"apk":    {"qemu-system-aarch64"},
		"brew":   {"qemu"},
	},
	"qemu-system-x86_64": {
		"apt":    {"qemu-system-x86"},
		"dnf":    {"qemu-system-x86"},
		"yum":    {"qemu-kvm"},
		"zypper": {"qemu-x86"},
		"pacman": {"qemu-base", "qemu-desktop", "qemu-full"},
		"apk":    {"qemu-system-x86_64"},
		"brew":   {"qemu"},
	},
	"qemu-img": {
		"apt":    {"qemu-utils"},
		"dnf":    {"qemu-img"},
		"yum":    {"qemu-img"},
		"zypper": {"qemu-tools"},
		// On Arch qemu-img is a package of its own and no system emulator
		// depends on it: package_qemu-system-aarch64 takes _qemu_system_deps,
		// edk2-aarch64 and systemd-libs, and neither that array nor the
		// qemu-common in it mentions qemu-img. package_qemu-base does depend
		// on it, which is why only the arm64 map, which names the emulator
		// directly, came up short.
		"pacman": {"qemu-img", "qemu-base", "qemu-desktop", "qemu-full"},
		"apk":    {"qemu-img"},
		"brew":   {"qemu"},
	},
	"docker": {
		"apt":    {"docker.io"},
		"dnf":    {"moby-engine", "docker-ce"},
		"yum":    {"docker", "docker-ce"},
		"zypper": {"docker"},
		"pacman": {"docker"},
		"apk":    {"docker"},
	},
	"podman": {
		"apt":    {"podman"},
		"dnf":    {"podman"},
		"yum":    {"podman"},
		"zypper": {"podman"},
		"pacman": {"podman"},
		"apk":    {"podman"},
	},
	"ip": {
		"apt":    {"iproute2"},
		"dnf":    {"iproute"},
		"yum":    {"iproute"},
		"zypper": {"iproute2"},
		"pacman": {"iproute2"},
		"apk":    {"iproute2"},
	},
}

func TestEveryDeclaredBinaryHasAPackageThatShipsIt(t *testing.T) {
	hosts := []struct {
		os   string
		arch string
		pms  []string
	}{
		{"linux", "amd64", []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"}},
		{"linux", "arm64", []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"}},
		{"darwin", "amd64", []string{"brew"}},
		{"darwin", "arm64", []string{"brew"}},
	}

	for _, host := range hosts {
		t.Run(host.os+"/"+host.arch, func(t *testing.T) {
			required := Required(platform.Info{OS: host.os, Arch: host.arch})
			for _, pm := range host.pms {
				for _, dep := range required {
					pkgs, err := InstallablePackages(pm, []Dependency{dep})
					if err != nil {
						t.Fatalf("%s: %v", pm, err)
					}
					for _, binary := range dep.Binaries {
						providers, ok := packageProviders[binary][pm]
						if !ok {
							t.Fatalf("test gap: no %s package is recorded as shipping %q", pm, binary)
						}
						if !slices.ContainsFunc(providers, func(p string) bool {
							return slices.Contains(pkgs, p)
						}) {
							t.Errorf("%s packages %v for dependency %q ship none of %v, so %q stays missing after setup installs them",
								pm, pkgs, dep.Name, providers, binary)
						}
					}
				}
			}
		})
	}
}

var linuxPackageManagers = []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"}

func TestRuntimeDepsCoverEveryLinuxPackageManager(t *testing.T) {
	for _, dep := range []Dependency{Docker(), Podman()} {
		for _, pm := range linuxPackageManagers {
			pkgs, err := InstallablePackages(pm, []Dependency{dep})
			if err != nil {
				t.Fatalf("%s: %v", dep.Name, err)
			}
			if len(pkgs) == 0 {
				t.Errorf("%s has no %s package", dep.Name, pm)
			}
			for _, binary := range dep.Binaries {
				providers, ok := packageProviders[binary][pm]
				if !ok {
					t.Fatalf("test gap: no %s package is recorded as shipping %q", pm, binary)
				}
				if !slices.ContainsFunc(providers, func(p string) bool {
					return slices.Contains(pkgs, p)
				}) {
					t.Errorf("%s packages %v for %q ship none of %v", pm, pkgs, dep.Name, providers)
				}
			}
			uninstall, err := UninstallablePackages(pm, []string{dep.Name}, []Dependency{dep})
			if err != nil || !slices.Equal(uninstall, pkgs) {
				t.Errorf("%s/%s: uninstall = %v, %v; want %v", dep.Name, pm, uninstall, err, pkgs)
			}
		}
	}
}

func TestRuntimeDepsNotInRequired(t *testing.T) {
	for _, info := range []platform.Info{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
	} {
		for _, dep := range Required(info) {
			if dep.Name == "docker" || dep.Name == "podman" {
				t.Errorf("%s/%s: Required includes %q, which setup must only install on request", info.OS, info.Arch, dep.Name)
			}
		}
	}
}
