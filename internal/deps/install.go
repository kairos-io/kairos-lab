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
	cmd, err := uninstallCommand(pm, packages, useSudo)
	if err != nil {
		return err
	}
	return run(cmd[0], cmd[1:]...)
}

func installCommands(pm string, pkgs []string, useSudo bool) ([][]string, error) {
	pre := []string{}
	if useSudo {
		pre = append(pre, "sudo")
	}
	switch pm {
	case "brew":
		return [][]string{append([]string{"brew", "install"}, pkgs...)}, nil
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

func uninstallCommand(pm string, pkgs []string, useSudo bool) ([]string, error) {
	pre := []string{}
	if useSudo {
		pre = append(pre, "sudo")
	}
	switch pm {
	case "brew":
		return append(pre, append([]string{"brew", "uninstall"}, pkgs...)...), nil
	case "apt":
		return append(pre, append([]string{"apt-get", "remove", "-y"}, pkgs...)...), nil
	case "dnf", "yum":
		return append(pre, append([]string{pm, "remove", "-y"}, pkgs...)...), nil
	case "zypper":
		return append(pre, append([]string{"zypper", "--non-interactive", "remove"}, pkgs...)...), nil
	case "pacman":
		return append(pre, append([]string{"pacman", "-R", "--noconfirm"}, pkgs...)...), nil
	case "apk":
		return append(pre, append([]string{"apk", "del"}, pkgs...)...), nil
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
