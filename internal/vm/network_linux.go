package vm

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/kairos-io/kairos-lab/internal/state"
)

const (
	DefaultBridgeName = "kairoslab0"
	DefaultTapName    = "kairoslab-tap0"
	resourceManagerNM = "manager:nm"
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

	// Clean up stale bridge from previous run if it exists and is causing issues
	if IsLinuxBridge(bridge) && st.Network.CreatedByKairosLab {
		if err := CleanupLinuxBridge(st); err != nil {
			return fmt.Errorf("cleanup stale bridge: %w", err)
		}
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
	if networkManagerActive() {
		if err := prepareLinuxBridgeWithNM(bridge, tap, uplink); err != nil {
			return err
		}
		created = append(created, resourceManagerNM)
	} else {
		markNetworkManagerUnmanaged(bridge)
		markNetworkManagerUnmanaged(tap)
		markNetworkManagerUnmanaged(uplink)
		if err := prepareLinuxBridgeManual(bridge, tap, uplink, &created); err != nil {
			return err
		}
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

func prepareLinuxBridgeManual(bridge, tap, uplink string, created *[]string) error {
	if !linkExists(bridge) {
		if err := sudo("ip", "link", "add", "name", bridge, "type", "bridge"); err != nil {
			return err
		}
		*created = append(*created, "bridge:"+bridge)
	}
	if err := sudo("ip", "link", "set", bridge, "up"); err != nil {
		return err
	}

	uplinkAddrs, err := ipv4AddrsOnInterface(uplink)
	if err != nil {
		return err
	}
	uplinkDefaultRoutes, err := defaultIPv4RoutesForDev(uplink)
	if err != nil {
		return err
	}

	for _, cidr := range uplinkAddrs {
		if err := sudo("ip", "-4", "addr", "replace", cidr, "dev", bridge); err != nil {
			return err
		}
	}
	if err := sudo("ip", "-4", "addr", "flush", "dev", uplink); err != nil {
		return err
	}
	if err := migrateDefaultRoutes(uplinkDefaultRoutes, uplink, bridge); err != nil {
		return err
	}

	if master, _ := currentMaster(uplink); master != bridge {
		if err := sudo("ip", "link", "set", uplink, "master", bridge); err != nil {
			return err
		}
		*created = append(*created, "uplink:"+uplink)
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
		*created = append(*created, "tap:"+tap)
	}
	if err := sudo("ip", "link", "set", tap, "master", bridge); err != nil {
		return err
	}
	if err := sudo("ip", "link", "set", tap, "up"); err != nil {
		return err
	}

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
	if networkManagerActive() || contains(st.Network.CreatedResources, resourceManagerNM) {
		return cleanupLinuxBridgeWithNM(st)
	}
	var errs []string
	if st.Network.TapName != "" {
		if err := sudo("ip", "link", "delete", st.Network.TapName); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if st.Network.BridgeInterface != "" && linkExists(st.Network.BridgeInterface) {
		bridgeAddrs := []string{}
		if st.Network.BridgeName != "" && linkExists(st.Network.BridgeName) {
			addrs, err := ipv4AddrsOnInterface(st.Network.BridgeName)
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				bridgeAddrs = addrs
			}
		}
		bridgeDefaultRoutes := []string{}
		if st.Network.BridgeName != "" {
			routes, err := defaultIPv4RoutesForDev(st.Network.BridgeName)
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				bridgeDefaultRoutes = routes
			}
		}
		if err := sudo("ip", "-4", "addr", "flush", "dev", st.Network.BridgeInterface); err != nil {
			errs = append(errs, err.Error())
		}
		if err := sudo("ip", "link", "set", st.Network.BridgeInterface, "nomaster"); err != nil {
			errs = append(errs, err.Error())
		}
		if err := sudo("ip", "link", "set", st.Network.BridgeInterface, "up"); err != nil {
			errs = append(errs, err.Error())
		}
		for _, cidr := range bridgeAddrs {
			if err := sudo("ip", "-4", "addr", "replace", cidr, "dev", st.Network.BridgeInterface); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if err := migrateDefaultRoutes(bridgeDefaultRoutes, st.Network.BridgeName, st.Network.BridgeInterface); err != nil {
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

func cleanupLinuxBridgeWithNM(st *state.State) error {
	bridgeConn := st.Network.BridgeName
	if bridgeConn == "" {
		bridgeConn = DefaultBridgeName
	}
	uplinkConn := bridgeConn + "-uplink"
	tapConn := bridgeConn + "-tap"

	var errs []string
	for _, conn := range []string{tapConn, uplinkConn, bridgeConn} {
		if !nmConnectionExists(conn) {
			continue
		}
		if err := sudo("nmcli", "connection", "delete", conn); err != nil {
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
				if iface != "" && iface != "lo" && !IsLinuxBridge(iface) {
					return iface, nil
				}
			}
		}
	}
	return "", fmt.Errorf("could not determine default uplink interface (all candidates are bridges or loopback)")
}

func ipv4AddrsOnInterface(iface string) ([]string, error) {
	out, err := exec.Command("ip", "-4", "-o", "addr", "show", "dev", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("list IPv4 addresses on %s: %w", iface, err)
	}
	return parseIPv4Addrs(string(out)), nil
}

func parseIPv4Addrs(output string) []string {
	set := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "inet" {
				set[fields[i+1]] = struct{}{}
				break
			}
		}
	}
	return sortedKeys(set)
}

func defaultIPv4RoutesForDev(iface string) ([]string, error) {
	out, err := exec.Command("ip", "-4", "route", "show", "default", "dev", iface).Output()
	if err != nil {
		return nil, fmt.Errorf("list default IPv4 routes on %s: %w", iface, err)
	}
	return parseRouteLines(string(out)), nil
}

func parseRouteLines(output string) []string {
	set := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = struct{}{}
	}
	return sortedKeys(set)
}

func migrateDefaultRoutes(routes []string, fromDev, toDev string) error {
	for _, route := range routes {
		rewritten, ok := rewriteRouteDevice(route, fromDev, toDev)
		if !ok {
			continue
		}
		args := append([]string{"-4", "route", "replace"}, strings.Fields(rewritten)...)
		if err := sudo("ip", args...); err != nil {
			return err
		}
	}
	return nil
}

func rewriteRouteDevice(route, fromDev, toDev string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(route))
	for i := 0; i < len(fields)-1; i++ {
		if fields[i] == "dev" && fields[i+1] == fromDev {
			fields[i+1] = toDev
			return strings.Join(fields, " "), true
		}
	}
	return route, false
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
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

func markNetworkManagerUnmanaged(iface string) {
	if iface == "" {
		return
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		return
	}
	_ = sudo("nmcli", "device", "set", iface, "managed", "no")
}

func contains(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
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
