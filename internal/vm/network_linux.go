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

// CanSuggestAutoBridged reports whether Linux bridged LAN mode is plausible for --network auto:
// NetworkManager is running, we resolved a wired default-route uplink, and it is not Wi‑Fi
// (NM/drivers seldom allow a wlan client NIC as a Linux bridge slave; see PrepareLinuxBridge).
func CanSuggestAutoBridged() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if !networkManagerActive() {
		return false
	}
	_, err := PreferWiredUplinkForBridge()
	return err == nil
}

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

	tap := st.Network.TapName
	if tap == "" {
		tap = DefaultTapName
	}

	uplink := st.Network.BridgeInterface
	if uplink == "" {
		// Retry a few times in case NM is still settling
		var u string
		var err error
		for i := 0; i < 5; i++ {
			u, err = PreferWiredUplinkForBridge()
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
	if isVirtualInterface(uplink) {
		return fmt.Errorf("uplink interface must be a physical host iface, got virtual interface: %s (use --bridge-if with Ethernet, e.g. enp*, eth*)", uplink)
	}
	if IfaceIsWLAN(uplink) {
		return fmt.Errorf("bridged LAN does not support Wi-Fi interface %q: NM/drivers seldom allow wlan client mode as a bridge slave; plug in Ethernet, pass --bridge-if <iface>, or use --network virbr / --network user instead", uplink)
	}

	// Always drop prior kairos-lab NM profiles (and tap/bridge if present). NetworkManager
	// permits duplicate profiles with the same name; deletes by name leave stale UUIDs
	// pinned to an old uplink (e.g. Ethernet) when you've moved interfaces. Recreate from
	// the interface chosen for this run (default route / -bridge-if).
	fmt.Println("Preparing bridged networking (resetting any prior kairos-lab bridge setup)...")
	if purgeLabBridgeNetworking(bridge, tap) {
		time.Sleep(2 * time.Second)
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

	// Tear down matching NM profiles inside this helper too: leftover profiles or
	// ambiguity on `modify`/`up NAME` still cause wrong-NIC binds (seen with enp* vs wlan).
	nmDeleteProfilesWithExactNames([]string{tapConn, uplinkConn, bridgeConn})

	if err := sudo("nmcli", "connection", "add", "type", "bridge", "ifname", bridge, "con-name", bridgeConn, "autoconnect", "yes", "stp", "no"); err != nil {
		return err
	}
	bridgeUUID, err := nmWaitSingletonUUIDAfterAdd(bridgeConn)
	if err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "modify", bridgeUUID,
		"connection.interface-name", bridge, "ipv4.method", "auto", "ipv6.method", "auto", "bridge.stp", "no", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	uplinkUUID, err := addEthernetBridgeSlaveUplink(uplink, uplinkConn, bridgeConn)
	if err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "modify", uplinkUUID,
		"connection.interface-name", uplink, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if err := sudo("nmcli", "connection", "add", "type", "tun", "ifname", tap, "con-name", tapConn, "mode", "tap", "owner", uid, "master", bridgeConn, "slave-type", "bridge", "autoconnect", "yes"); err != nil {
		return err
	}
	tapUUID, err := nmWaitSingletonUUIDAfterAdd(tapConn)
	if err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "modify", tapUUID,
		"connection.interface-name", tap, "tun.mode", "tap", "tun.owner", uid, "master", bridgeConn, "slave-type", "bridge", "connection.autoconnect", "yes"); err != nil {
		return err
	}

	if err := sudo("nmcli", "connection", "up", bridgeUUID, "ifname", bridge); err != nil {
		return err
	}
	if err := sudo("nmcli", "connection", "up", uplinkUUID, "ifname", uplink); err != nil {
		return fmt.Errorf("activate bridge slave uplink %q: %w", uplink, err)
	}
	if err := sudo("nmcli", "connection", "up", tapUUID, "ifname", tap); err != nil {
		return err
	}
	return nil
}

// IfaceIsWLAN reports sysfs-style wireless NICs (not used as bridged LAN uplinks here).
func IfaceIsWLAN(name string) bool {
	if name == "" {
		return false
	}
	base := filepath.Join("/sys/class/net", name)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		return true
	}
	return strings.HasPrefix(name, "wl")
}

// PreferWiredUplinkForBridge returns the first default-route NIC that is not classified as wireless.
func PreferWiredUplinkForBridge() (string, error) {
	for _, iface := range DetectUplinkCandidates() {
		if !IfaceIsWLAN(iface) {
			return iface, nil
		}
	}
	return "", fmt.Errorf("no wired (Ethernet) default-route interface — bridged LAN is not supported on Wi-Fi-only hosts here; plug in Ethernet and use --network bridged, or use --network virbr / --network user")
}

func addEthernetBridgeSlaveUplink(uplinkIface, uplinkConnName, bridgeConnName string) (string, error) {
	if err := sudo("nmcli", "connection", "add", "type", "ethernet",
		"ifname", uplinkIface,
		"con-name", uplinkConnName,
		"master", bridgeConnName,
		"slave-type", "bridge",
		"autoconnect", "yes"); err != nil {
		return "", err
	}
	return nmWaitSingletonUUIDAfterAdd(uplinkConnName)
}

// parseNmCliColonField parses -t or verbose nmcli sections as KEY[:][VALUE]. Key match is ASCII case-insensitive.
func parseNmCliColonField(output, wantedKey string) (value string, found bool) {
	wantedKey = strings.TrimSpace(wantedKey)
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), wantedKey) {
			continue
		}
		return strings.TrimSpace(val), true
	}
	return "", false
}

func CleanupLinuxBridge(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !st.Network.CreatedByKairosLab || st.Network.Mode != "bridged" {
		return nil
	}
	bridgeConn := st.Network.BridgeName
	if bridgeConn == "" {
		bridgeConn = DefaultBridgeName
	}
	tapIface := st.Network.TapName
	if tapIface == "" {
		tapIface = DefaultTapName
	}
	if err := cleanupNMConnections(bridgeConn, tapIface); err != nil {
		return err
	}
	st.Network.LastCleanupAttemptAt = state.NowRFC3339()
	st.Network = state.Network{}
	return nil
}

// HasUsableLinuxVirbr reports whether libvirt's default NAT bridge exists, is UP, is a Linux bridge,
// and has an IPv4 address (typical dnsmasq on 192.168.122.1).
func HasUsableLinuxVirbr() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if !linkExists(LinuxLibvirtNATBridge) || !IsLinuxBridge(LinuxLibvirtNATBridge) {
		return false
	}
	out, err := exec.Command("ip", "link", "show", "dev", LinuxLibvirtNATBridge).CombinedOutput()
	if err != nil {
		return false
	}
	if !strings.Contains(string(out), " state UP ") && !strings.Contains(string(out), " state UNKNOWN ") {
		return false
	}
	addrOut, err := exec.Command("ip", "-4", "-o", "addr", "show", "dev", LinuxLibvirtNATBridge).Output()
	return err == nil && strings.Contains(string(addrOut), " inet ")
}

func tapOwnershipUser() (string, error) {
	u := strings.TrimSpace(os.Getenv("SUDO_USER"))
	if u != "" && u != "root" {
		return u, nil
	}
	u = strings.TrimSpace(os.Getenv("USER"))
	if u != "" && u != "root" {
		return u, nil
	}
	return "", fmt.Errorf("cannot determine a non-root user for QEMU tap ownership (avoid running kairos-lab entirely as root)")
}

// PrepareLinuxVirbrTap attaches a QEMU tap device to LinuxLibvirtNATBridge (typically virbr0).
// Requires sudo for ip-link / tuntap; qemu can run without sudo once the tap is owned by tapOwnershipUser().
func PrepareLinuxVirbrTap(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !HasUsableLinuxVirbr() {
		return fmt.Errorf(`libvirt default bridge "%s" is unavailable (missing, down, or no IPv4) — ensure libvirt NAT is active (often "sudo virsh net-start default"), then retry, or use --network user`, LinuxLibvirtNATBridge)
	}
	tap := DefaultVirbrTapName
	owner, err := tapOwnershipUser()
	if err != nil {
		return err
	}
	fmt.Printf("Preparing libvirt NAT tap %q on bridge %s (for user %s)...\n", tap, LinuxLibvirtNATBridge, owner)

	if linkExists(tap) {
		_ = sudo("ip", "link", "set", tap, "nomaster")
		if err := sudo("ip", "link", "delete", tap); err != nil {
			return fmt.Errorf("remove existing tap %s: %w", tap, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := sudo("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", owner); err != nil {
		return err
	}
	if err := sudo("ip", "link", "set", "dev", tap, "master", LinuxLibvirtNATBridge); err != nil {
		_ = sudo("ip", "link", "delete", tap)
		return err
	}
	if err := sudo("ip", "link", "set", "dev", tap, "up"); err != nil {
		_ = sudo("ip", "link", "set", tap, "nomaster")
		_ = sudo("ip", "link", "delete", tap)
		return err
	}

	st.Network.Mode = "virbr"
	st.Network.BridgeName = LinuxLibvirtNATBridge
	st.Network.TapName = tap
	st.Network.BridgeInterface = ""
	st.Network.CleanupRequired = true
	st.Network.CreatedByKairosLab = true
	st.Network.LastPreparedAt = state.NowRFC3339()
	return nil
}

// CleanupLinuxVirbrTap removes TAP state created by PrepareLinuxVirbrTap.
func CleanupLinuxVirbrTap(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if !st.Network.CreatedByKairosLab || st.Network.Mode != "virbr" {
		return nil
	}
	tap := st.Network.TapName
	if tap == "" {
		tap = DefaultVirbrTapName
	}
	st.Network.LastCleanupAttemptAt = state.NowRFC3339()
	if linkExists(tap) {
		_ = sudo("ip", "link", "set", tap, "nomaster")
		if err := sudo("ip", "link", "delete", tap); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to delete tap %s: %v\n", tap, err)
		}
	}
	st.Network = state.Network{}
	return nil
}

func staleUnusedVirbrTap() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return linkExists(DefaultVirbrTapName)
}

func removeVirbrNATTapIfUnused() {
	if runtime.GOOS != "linux" || !linkExists(DefaultVirbrTapName) {
		return
	}
	_ = sudo("ip", "link", "set", DefaultVirbrTapName, "nomaster")
	if err := sudo("ip", "link", "delete", DefaultVirbrTapName); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to delete stale tap %s: %v\n", DefaultVirbrTapName, err)
	}
}

// HasStaleNetworkResources checks if there are kairos-lab network resources
// that exist but aren't tracked in state (e.g., from a failed setup).
func HasStaleNetworkResources(st *state.State) bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if st.Network.CreatedByKairosLab {
		return false
	}
	bridge := st.Network.BridgeName
	if bridge == "" {
		bridge = DefaultBridgeName
	}
	return IsLinuxBridge(bridge) || nmConnectionExists(bridge) ||
		nmConnectionExists(bridge+"-uplink") || nmConnectionExists(bridge+"-tap") ||
		staleUnusedVirbrTap()
}

// CleanupStaleNetworkResources removes kairos-lab network resources that aren't
// tracked in state, typically from a failed or interrupted setup.
func CleanupStaleNetworkResources(st *state.State) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	bridge := st.Network.BridgeName
	if bridge == "" {
		bridge = DefaultBridgeName
	}
	tapIface := DefaultTapName
	if err := cleanupNMConnections(bridge, tapIface); err != nil {
		return err
	}
	removeVirbrNATTapIfUnused()
	return nil
}

// purgeLabBridgeNetworking removes kairos-lab NetworkManager profiles and tap/bridge
// interfaces. It deletes every NM connection UUID whose name matches the lab bridge,
// uplink slave, or tap slave (handles duplicate nmcli connection names). Returns whether
// any resource was removed.
func purgeLabBridgeNetworking(bridgeConn, tapIface string) bool {
	uplinkConn := bridgeConn + "-uplink"
	tapConn := bridgeConn + "-tap"
	names := []string{tapConn, uplinkConn, bridgeConn}

	changed := false

	// Find the physical interface enslaved to the bridge before we delete anything
	var uplinkIface string
	if IsLinuxBridge(bridgeConn) {
		uplinkIface = findBridgeSlave(bridgeConn)
	}

	if nmDeleteProfilesWithExactNames(names) {
		changed = true
	}

	// Now clean up any lingering interfaces that NM didn't remove
	if linkExists(tapIface) {
		if err := sudo("ip", "link", "delete", tapIface); err != nil {
			fmt.Printf("warning: failed to delete interface %s: %v\n", tapIface, err)
		} else {
			changed = true
		}
	}
	if linkExists(bridgeConn) {
		if err := sudo("ip", "link", "delete", bridgeConn); err != nil {
			fmt.Printf("warning: failed to delete interface %s: %v\n", bridgeConn, err)
		} else {
			changed = true
		}
	}

	// Reconnect the physical interface (NM connections are gone, so this
	// will use a fresh/default connection, not the bridge slave profile)
	if uplinkIface != "" {
		fmt.Printf("Reconnecting %s...\n", uplinkIface)
		if err := sudo("nmcli", "device", "connect", uplinkIface); err != nil {
			fmt.Printf("warning: failed to reconnect %s: %v\n", uplinkIface, err)
		}
	}

	return changed
}

func cleanupNMConnections(bridgeConn, tapIface string) error {
	_ = purgeLabBridgeNetworking(bridgeConn, tapIface)
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

// DetectUplinkCandidates returns all valid physical interfaces from the default routes.
// The list preserves the order returned by `ip route show default` after filtering and de-duplication.
func DetectUplinkCandidates() []string {
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var candidates []string
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				iface := fields[i+1]
				if iface != "" && iface != "lo" && !IsLinuxBridge(iface) && !isVirtualInterface(iface) && !seen[iface] {
					seen[iface] = true
					candidates = append(candidates, iface)
				}
			}
		}
	}
	return candidates
}

func isVirtualInterface(name string) bool {
	virtualPrefixes := []string{
		"docker",  // Docker default bridge
		"br-",     // Docker custom networks
		"veth",    // Virtual ethernet (container endpoints)
		"virbr",   // libvirt bridges
		"vnet",    // libvirt VM interfaces
		"lxcbr",   // LXC bridges
		"lxdbr",   // LXD bridges
		"cni",     // Kubernetes CNI
		"flannel", // Kubernetes flannel
		"calico",  // Kubernetes calico
		"weave",   // Kubernetes weave
		"tunl",    // IPIP tunnels
		"podman",  // Podman networks
	}
	for _, prefix := range virtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
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

// nmConnectionUUIDsForExactName returns every NM connection UUID whose connection
// NAME equals exactName (NetworkManager permits duplicate names with different UUIDs).
func nmConnectionUUIDsForExactName(exactName string) []string {
	if exactName == "" {
		return nil
	}
	if _, err := exec.LookPath("nmcli"); err != nil {
		return nil
	}
	out, err := exec.Command("nmcli", "-t", "-f", "UUID,NAME", "connection", "show").Output()
	if err != nil {
		return nil
	}
	return parseNmcliUUIDNamePairs(string(out), exactName)
}

// parseNmcliUUIDNamePairs parses "nmcli -t -f UUID,NAME connection show" output lines
// and returns UUIDs for rows whose NAME matches exactName after the first ':'.
func parseNmcliUUIDNamePairs(output, exactName string) []string {
	var match []string
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i <= 0 || i >= len(line)-1 {
			continue
		}
		uuid := line[:i]
		connName := line[i+1:]
		if connName != exactName {
			continue
		}
		if uuid != "" {
			match = append(match, uuid)
		}
	}
	return match
}

// nmDeleteProfilesWithExactNames deletes every NM connection whose NAME matches exactly.
func nmDeleteProfilesWithExactNames(names []string) bool {
	removed := false
	for _, nmName := range names {
		for _, uuid := range nmConnectionUUIDsForExactName(nmName) {
			if err := sudo("nmcli", "connection", "delete", uuid); err != nil {
				fmt.Printf("warning: failed to delete connection %s: %v\n", uuid, err)
				continue
			}
			removed = true
		}
	}
	return removed
}

func nmWaitSingletonUUIDAfterAdd(nmName string) (string, error) {
	var lastErr error
	for range 40 {
		uuids := nmConnectionUUIDsForExactName(nmName)
		switch len(uuids) {
		case 1:
			return uuids[0], nil
		case 0:
			lastErr = fmt.Errorf("no NetworkManager connection named %q after create", nmName)
		default:
			return "", fmt.Errorf("ambiguous NetworkManager profiles named %q (%d); run kairos-lab cleanup and retry", nmName, len(uuids))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", lastErr
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
