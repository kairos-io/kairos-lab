package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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
	if !networkManagerActive() {
		return fmt.Errorf("NetworkManager is required for bridged networking on Linux. Please install and enable NetworkManager, or use --network user for port-forwarded access")
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}
	bridge := st.Network.BridgeName
	if bridge == "" {
		bridge = DefaultBridgeName
	}

	// Check for stale bridge from previous run
	if IsLinuxBridge(bridge) || nmConnectionExists(bridge) {
		fmt.Println("Found stale network configuration, cleaning up...")
		
		// First, find the physical interface that's enslaved to the bridge
		uplinkIface := findBridgeSlave(bridge)
		
		// Delete our NM connections
		cleanupNMConnections(bridge)
		
		// If we found an enslaved interface, reconnect it
		if uplinkIface != "" {
			fmt.Printf("Reconnecting %s...\n", uplinkIface)
			// Try to bring up the device - NM will auto-connect or we trigger DHCP
			_ = sudo("nmcli", "device", "connect", uplinkIface)
			time.Sleep(3 * time.Second)
		} else {
			time.Sleep(2 * time.Second)
		}
	}

	uplink := st.Network.BridgeInterface
	if uplink == "" {
		// Retry a few times in case NM is still restoring the connection
		var u string
		var err error
		for i := 0; i < 5; i++ {
			u, err = detectDefaultUplink()
			if err == nil {
				break
			}
			time.Sleep(time.Second)
		}
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

	if err := prepareLinuxBridgeWithNM(bridge, tap, uplink); err != nil {
		return err
	}

	st.Network.Mode = "bridged"
	st.Network.BridgeName = bridge
	st.Network.BridgeInterface = uplink
	st.Network.TapName = tap
	st.Network.CleanupRequired = true
	st.Network.CreatedByKairosLab = true
	st.Network.LastPreparedAt = state.NowRFC3339()
	return nil
}

func prepareLinuxBridgeWithNM(bridge, tap, uplink string) error {
	if _, err := exec.LookPath("nmcli"); err != nil {
		return fmt.Errorf("NetworkManager is active but nmcli is not installed")
	}
	bridgeConn := bridge
	uplinkConn := bridge + "-uplink"
	tapConn := bridge + "-tap"
	user := os.Getenv("SUDO_USER")
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		user = "root"
	}
	uid, err := uidForUser(user)
	if err != nil {
		return err
	}

	if !nmConnectionExists(bridgeConn) {
		if err := sudo("nmcli", "connection", "add", "type", "bridge", "ifname", bridge, "con-name", bridgeConn, "autoconnect", "yes", "stp", "no"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", bridgeConn, "connection.interface-name", bridge, "ipv4.method", "auto", "ipv6.method", "auto", "bridge.stp", "no", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if !nmConnectionExists(uplinkConn) {
		if err := sudo("nmcli", "connection", "add", "type", "ethernet", "ifname", uplink, "con-name", uplinkConn, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "yes"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", uplinkConn, "connection.interface-name", uplink, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if !nmConnectionExists(tapConn) {
		if err := sudo("nmcli", "connection", "add", "type", "tun", "ifname", tap, "con-name", tapConn, "mode", "tap", "owner", uid, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "yes"); err != nil {
			return err
		}
	}
	if err := sudo("nmcli", "connection", "modify", tapConn, "connection.interface-name", tap, "tun.mode", "tap", "tun.owner", uid, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if err := sudo("nmcli", "connection", "up", bridgeConn); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", uplinkConn); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", tapConn); err != nil {
		return err
	}
	return nil
}

func CleanupLinuxBridge(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !st.Network.CreatedByKairosLab {
		return nil
	}
	bridgeConn := st.Network.BridgeName
	if bridgeConn == "" {
		bridgeConn = DefaultBridgeName
	}
	if err := cleanupNMConnections(bridgeConn); err != nil {
		return err
	}
	st.Network.LastCleanupAttemptAt = state.NowRFC3339()
	st.Network = state.Network{}
	return nil
}

func cleanupNMConnections(bridgeConn string) error {
	uplinkConn := bridgeConn + "-uplink"
	tapConn := bridgeConn + "-tap"

	// Delete all connections with these names (there might be duplicates)
	for _, conn := range []string{tapConn, uplinkConn, bridgeConn} {
		// Keep deleting while connection exists
		for nmConnectionExists(conn) {
			_ = sudoQuiet("nmcli", "connection", "delete", conn)
		}
	}
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

// findBridgeSlave finds a physical interface enslaved to the given bridge
func findBridgeSlave(bridge string) string {
	// List interfaces that have this bridge as master
	out, err := exec.Command("ip", "-o", "link", "show", "master", bridge).Output()
	if err != nil {
		return ""
	}
	// Parse output to find non-tap interfaces
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Format: "3: enp0s31f6: <...>"
		iface := strings.TrimSuffix(fields[1], ":")
		// Skip tap interfaces
		if strings.Contains(iface, "tap") {
			continue
		}
		return iface
	}
	return ""
}

func sudo(name string, args ...string) error {
	argv := append([]string{name}, args...)
	cmd := exec.Command("sudo", argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo command failed: sudo %s: %w", strings.Join(argv, " "), err)
	}
	return nil
}

// sudoQuiet runs a sudo command without showing output (for cleanup operations)
func sudoQuiet(name string, args ...string) error {
	argv := append([]string{name}, args...)
	cmd := exec.Command("sudo", argv...)
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo command failed: sudo %s: %w", strings.Join(argv, " "), err)
	}
	return nil
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
				if iface != "" && iface != "lo" && !IsLinuxBridge(iface) {
					return iface, nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not determine default uplink interface (all candidates are bridges or loopback)")
}

func networkManagerActive() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	return exec.Command("systemctl", "is-active", "--quiet", "NetworkManager").Run() == nil
}

func nmConnectionExists(name string) bool {
	if name == "" {
		return false
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		return false
	}
	return exec.Command("nmcli", "-t", "-f", "NAME", "connection", "show", name).Run() == nil
}

func uidForUser(user string) (string, error) {
	out, err := exec.Command("id", "-u", user).Output()
	if err != nil {
		return "", fmt.Errorf("resolve uid for user %q: %w", user, err)
	}
	uid := strings.TrimSpace(string(out))
	if uid == "" {
		return "", fmt.Errorf("resolve uid for user %q: empty uid", user)
	}
	for _, r := range uid {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("resolve uid for user %q: invalid uid %q", user, uid)
		}
	}
	return uid, nil
}
