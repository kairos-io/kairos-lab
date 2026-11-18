package deps

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func Install(pm string, packages []string, useSudo bool) error {
	if len(packages) == 0 {
		return nil
	}
	commands, err := installCommands(pm, packages, useSudo)
	if err != nil {
		return err
	}
	for _, c := range commands {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func Uninstall(pm string, packages []string, useSudo bool) error {
	if len(packages) == 0 {
		return nil
	}
	commands, err := uninstallCommands(pm, packages, useSudo)
	if err != nil {
		return err
	}
	for _, c := range commands {
		if err := run(c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// UninstallSideEffect names a dependency cleanup that reaches past the
// packages being removed, so the cleanup plan can say so before the user
// confirms it. It is empty for a package manager whose cleanup is scoped to
// the removal, which is all of them except apt.
func UninstallSideEffect(pm string) string {
	if pm == "apt" {
		return "apt-get autoremove runs afterwards; it removes every package apt has marked as an unused automatic dependency, not only the ones above"
	}
	return ""
}

func installCommands(pm string, pkgs []string, useSudo bool) ([][]string, error) {
	pre := []string{}
	if useSudo {
		pre = append(pre, "sudo")
	}
	switch pm {
	case "brew":
		return [][]string{append([]string{"brew", "install"}, pkgs...)}, nil
	case BrewCask:
		return [][]string{append([]string{"brew", "install", "--cask"}, pkgs...)}, nil
	case "apt":
		return [][]string{
			append(append([]string{}, pre...), "apt-get", "update"),
			append(append([]string{}, pre...), append([]string{"apt-get", "install", "-y"}, pkgs...)...),
		}, nil
	case "dnf", "yum":
		return [][]string{append(append([]string{}, pre...), append([]string{pm, "install", "-y"}, pkgs...)...)}, nil
	case "zypper":
		return [][]string{append(append([]string{}, pre...), append([]string{"zypper", "--non-interactive", "install"}, pkgs...)...)}, nil
	case "pacman":
		return [][]string{append(append([]string{}, pre...), append([]string{"pacman", "-S", "--noconfirm"}, pkgs...)...)}, nil
	case "apk":
		return [][]string{append(append([]string{}, pre...), append([]string{"apk", "add"}, pkgs...)...)}, nil
	default:
		return nil, fmt.Errorf("unsupported package manager: %s", pm)
	}
}

// uninstallCommands removes the named packages AND the dependencies that
// installing them pulled in, which nothing else needs now. Removing only what
// is named leaves the dependency behind, and on apt that dependency can be the
// binary itself: `apt-get remove docker.io` keeps docker-cli, containerd, runc
// and docker-buildx, so `docker` is still on PATH after cleanup, and the next
// setup finds it and records docker as pre-existing.
//
// Every manager but apt can scope the cleanup to the removal, so that is what
// they are told to do. apt has no scoped form: its autoremover is a separate
// pass over everything marked automatic. It is run as its own command rather
// than folded into `apt-get autoremove <pkgs>` because older apt refuses
// package arguments there, and UninstallSideEffect puts the wider reach into
// the cleanup plan before the user confirms it.
//
// brew is left alone on purpose. The only cask setup installs is
// docker-desktop and casks carry no dependencies, so the bug above cannot
// happen; `brew autoremove` is global like apt's, with nothing to gain here.
func uninstallCommands(pm string, pkgs []string, useSudo bool) ([][]string, error) {
	pre := []string{}
	if useSudo {
		pre = append(pre, "sudo")
	}
	one := func(args ...string) [][]string {
		return [][]string{append(append([]string{}, pre...), append(args, pkgs...)...)}
	}
	switch pm {
	case "brew":
		return one("brew", "uninstall"), nil
	case BrewCask:
		return one("brew", "uninstall", "--cask"), nil
	case "apt":
		return [][]string{
			append(append([]string{}, pre...), append([]string{"apt-get", "remove", "-y"}, pkgs...)...),
			append(append([]string{}, pre...), "apt-get", "autoremove", "-y"),
		}, nil
	case "dnf", "yum":
		// Fedora turns clean_requirements_on_remove on by default and RHEL
		// does not, so it is set here instead of being relied on.
		return one(pm, "remove", "-y", "--setopt=clean_requirements_on_remove=1"), nil
	case "zypper":
		return one("zypper", "--non-interactive", "remove", "--clean-deps"), nil
	case "pacman":
		return one("pacman", "-Rs", "--noconfirm"), nil
	case "apk":
		// apk del already removes dependencies nothing else needs.
		return one("apk", "del"), nil
	default:
		return nil, fmt.Errorf("unsupported package manager: %s", pm)
	}
}

func run(name string, args ...string) error {
	fmt.Printf("Running: %s %s\n", name, strings.Join(args, " "))
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command failed: %s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}
