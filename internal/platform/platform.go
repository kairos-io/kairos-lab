package platform

import (
	"fmt"
	"os/exec"
	"runtime"
)

type Info struct {
	OS             string
	Arch           string
	PackageManager string
}

func Detect() Info {
	pm, _ := DetectPackageManager(runtime.GOOS)
	return Info{
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
		PackageManager: pm,
	}
}

func DetectPackageManager(goos string) (string, error) {
	if goos == "darwin" {
		if HasCommand("brew") {
			return "brew", nil
		}
		return "", fmt.Errorf("homebrew not found")
	}
	if goos != "linux" {
		return "", fmt.Errorf("unsupported operating system: %s", goos)
	}
	for _, candidate := range []string{"apt", "dnf", "yum", "zypper", "pacman", "apk"} {
		if HasCommand(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no supported package manager found")
}

func HasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
