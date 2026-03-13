package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/kairos-io/kairos-lab/internal/state"
)

const (
	DefaultBridgeName = "kairoslab0"
	DefaultTapName    = "kairoslab-tap0"
	DefaultBridgeCIDR = "192.168.76.1/24"
	DefaultDHCPRange  = "192.168.76.50,192.168.76.150,12h"
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
	tap := st.Network.TapName
	if tap == "" {
		tap = DefaultTapName
	}
	pidFile := st.Network.DHCPPIDFile
	if pidFile == "" {
		pidFile = filepath.Join(runtimeDir, "dnsmasq.pid")
	}
	leaseFile := st.Network.DHCPLeaseFile
	if leaseFile == "" {
		leaseFile = filepath.Join(runtimeDir, "dnsmasq.leases")
	}

	created := []string{}
	if !linkExists(bridge) {
		if err := sudo("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			return err
		}
		created = append(created, "bridge:"+bridge)
		if err := sudo("ip", "addr", "add", DefaultBridgeCIDR, "dev", bridge); err != nil {
			return err
		}
		if err := sudo("ip", "link", "set", bridge, "up"); err != nil {
			return err
		}
	}

	if !linkExists(tap) {
		user := os.Getenv("USER")
		if user == "" {
			user = "root"
		}
		if err := sudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", user); err != nil {
			return err
		}
		created = append(created, "tap:"+tap)
		if err := sudo("ip", "link", "set", tap, "master", bridge); err != nil {
			return err
		}
		if err := sudo("ip", "link", "set", tap, "up"); err != nil {
			return err
		}
	}

	if !dnsmasqRunning(pidFile) {
		if err := sudo("dnsmasq",
			"--interface="+bridge,
			"--bind-interfaces",
			"--except-interface=lo",
			"--dhcp-range="+DefaultDHCPRange,
			"--dhcp-leasefile="+leaseFile,
			"--pid-file="+pidFile,
		); err != nil {
			return err
		}
		created = append(created, "dnsmasq:"+pidFile)
	}

	st.Network.Mode = "bridged"
	st.Network.BridgeName = bridge
	st.Network.TapName = tap
	st.Network.DHCPPIDFile = pidFile
	st.Network.DHCPLeaseFile = leaseFile
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
	if st.Network.DHCPPIDFile != "" {
		pidBytes, err := os.ReadFile(st.Network.DHCPPIDFile)
		if err == nil {
			pid := strings.TrimSpace(string(pidBytes))
			if pid != "" {
				_ = sudo("kill", pid)
			}
		}
		_ = os.Remove(st.Network.DHCPPIDFile)
	}
	if st.Network.DHCPLeaseFile != "" {
		_ = os.Remove(st.Network.DHCPLeaseFile)
	}
	if st.Network.TapName != "" {
		if err := sudo("ip", "link", "delete", st.Network.TapName); err != nil {
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

func dnsmasqRunning(pidFile string) bool {
	if pidFile == "" {
		return false
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
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
