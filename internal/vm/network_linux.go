package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kairos-io/kairos-lab/internal/state"
)

const (
	DefaultBridgeName = "kairoslab0"
	DefaultTapName    = "kairoslab-tap0"
)

func PrepareLinuxBridge(st *state.State, runtimeDir string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	bridge := st.Network.BridgeName
	if bridge == "" {
		bridge = DefaultBridgeName
	}
	uplink := st.Network.BridgeInterface
	if uplink == "" {
		u, err := detectDefaultUplink()
		if err != nil {
			return err
		}
		uplink = u
	}
	if uplink == bridge {
		return fmt.Errorf("uplink interface and bridge cannot be the same: %s", uplink)
	}
	if !linkExists(uplink) {
		return fmt.Errorf("uplink interface does not exist: %s", uplink)
	}
	if IsLinuxBridge(uplink) {
		return fmt.Errorf("uplink interface must be a physical host iface, got bridge: %s", uplink)
	}

	tap := st.Network.TapName
	if tap == "" {
		tap = DefaultTapName
	}

	created := []string{}
	if !linkExists(bridge) {
		if err := sudo("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			return err
		}
		created = append(created, "bridge:"+bridge)
	}
	if err := sudo("ip", "link", "set", bridge, "up"); err != nil {
		return err
	}
	if master, _ := currentMaster(uplink); master != bridge {
		if err := sudo("ip", "link", "set", uplink, "master", bridge); err != nil {
			return err
		}
		created = append(created, "uplink:"+uplink)
	}
	if err := sudo("ip", "link", "set", uplink, "up"); err != nil {
		return err
	}

	if !linkExists(tap) {
		user := os.Getenv("SUDO_USER")
		if user == "" {
			user = os.Getenv("USER")
		}
		if user == "" {
			user = "root"
		}
		if err := sudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", user); err != nil {
			return err
		}
		created = append(created, "tap:"+tap)
	}
	if err := sudo("ip", "link", "set", tap, "master", bridge); err != nil {
		return err
	}
	if err := sudo("ip", "link", "set", tap, "up"); err != nil {
		return err
	}

	st.Network.Mode = "bridged"
	st.Network.BridgeName = bridge
	st.Network.BridgeInterface = uplink
	st.Network.TapName = tap
	st.Network.DHCPPIDFile = ""
	st.Network.DHCPLeaseFile = ""
	st.Network.CleanupRequired = true
	st.Network.CreatedByKairosLab = true
	st.Network.CreatedResources = unique(append(st.Network.CreatedResources, created...))
	st.Network.LastPreparedAt = state.NowRFC3339()
	return nil
}

func CleanupLinuxBridge(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !st.Network.CreatedByKairosLab {
		return nil
	}
	var errs []string
	if st.Network.TapName != "" {
		if err := sudo("ip", "link", "delete", st.Network.TapName); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if st.Network.BridgeInterface != "" && linkExists(st.Network.BridgeInterface) {
		if err := sudo("ip", "link", "set", st.Network.BridgeInterface, "nomaster"); err != nil {
			errs = append(errs, err.Error())
		}
		if err := sudo("ip", "link", "set", st.Network.BridgeInterface, "up"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if st.Network.BridgeName != "" {
		if err := sudo("ip", "link", "set", st.Network.BridgeName, "down"); err != nil {
			errs = append(errs, err.Error())
		}
		if err := sudo("ip", "link", "delete", st.Network.BridgeName, "type", "bridge"); err != nil {
			errs = append(errs, err.Error())
		}
	}
	st.Network.LastCleanupAttemptAt = state.NowRFC3339()
	if len(errs) > 0 {
		return fmt.Errorf("bridge cleanup failed: %s", strings.Join(errs, "; "))
	}
	st.Network = state.Network{}
	return nil
}

func linkExists(name string) bool {
	if name == "" {
		return false
	}
	cmd := exec.Command("ip", "link", "show", name)
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func sudo(name string, args ...string) error {
	argv := append([]string{name}, args...)
	cmd := exec.Command("sudo", argv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo command failed: sudo %s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func unique(values []string) []string {
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
	return out
}

func IsPathGone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func IsLinuxBridge(name string) bool {
	if runtime.GOOS != "linux" || name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join("/sys/class/net", name, "bridge"))
	return err == nil
}

func currentMaster(iface string) (string, bool) {
	masterPath := filepath.Join("/sys/class/net", iface, "master")
	target, err := os.Readlink(masterPath)
	if err != nil {
		return "", false
	}
	return filepath.Base(target), true
}

func detectDefaultUplink() (string, error) {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return "", fmt.Errorf("detect default uplink: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				iface := fields[i+1]
				if iface != "" && iface != "lo" {
					return iface, nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not determine default uplink interface")
}
