package deps

import "testing"

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
