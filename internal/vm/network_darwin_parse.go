package vm

import (
	"fmt"
	"slices"
	"strings"
)

// The parsing in this file is deliberately kept apart from the exec calls in
// network_darwin.go so it can be exercised on any host, not only on macOS.

// darwinVirtualPrefixes lists the interface names vmnet-bridged cannot bridge
// onto: loopback, tunnels, and the virtual devices macOS creates for AirDrop,
// internet sharing and earlier kairos-lab runs.
var darwinVirtualPrefixes = []string{
	"lo",     // loopback
	"gif",    // generic tunnel
	"stf",    // 6to4 tunnel
	"utun",   // VPN and system tunnels
	"awdl",   // Apple Wireless Direct Link (AirDrop)
	"llw",    // low-latency WLAN
	"ap",     // Wi-Fi access point (internet sharing)
	"bridge", // bridges, including the bridge100 vmnet builds
	"vmenet", // vmnet guest endpoints
	"p2p",    // peer-to-peer Wi-Fi
	"anpi",   // Apple internal network processor
	"vlan",   // VLAN pseudo-interfaces
	"feth",   // fake ethernet pairs
}

func isDarwinVirtualInterface(name string) bool {
	for _, prefix := range darwinVirtualPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// parseDarwinDefaultRouteIface returns the interface named by the "interface:"
// line of `route -n get default`, or "" when the output carries none.
func parseDarwinDefaultRouteIface(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || key != "interface" {
			continue
		}
		return strings.TrimSpace(value)
	}
	return ""
}

// parseDarwinIfaceList splits the single space-separated line `ifconfig -l`
// prints into interface names.
func parseDarwinIfaceList(out string) []string {
	return strings.Fields(out)
}

// parseDarwinIfaceStatus returns the value of the "status:" line of
// `ifconfig <iface>`, lowercased. Interfaces that report no status at all
// (loopback and most virtual devices) give "".
func parseDarwinIfaceStatus(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found || key != "status" {
			continue
		}
		return strings.ToLower(strings.TrimSpace(value))
	}
	return ""
}

// parseDarwinHardwarePorts maps BSD device name to hardware port name from
// `networksetup -listallhardwareports`, e.g. "en1" -> "Wi-Fi".
func parseDarwinHardwarePorts(out string) map[string]string {
	ports := map[string]string{}
	port := ""
	for _, line := range strings.Split(out, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Hardware Port":
			port = value
		case "Device":
			if port != "" && value != "" {
				ports[value] = port
			}
			port = ""
		}
	}
	return ports
}

// isWiFiPort reports whether a hardware port name is a Wi-Fi radio. macOS has
// called the same port "AirPort" and "Wi-Fi" across releases.
func isWiFiPort(port string) bool {
	port = strings.ToLower(port)
	return strings.Contains(port, "wi-fi") || strings.Contains(port, "airport")
}

// The two ways out of a bridge validation error, worded for where the user is
// standing when they read it. On the command line they are flags. Inside the
// config review the flags are already spent, and the menu options are what the
// user can still reach (kairos-io/kairos#4649).
const (
	FlagBridgeControls   = "use -bridge-if to pick one, or -network user for port-forwarded access"
	ReviewBridgeControls = "pick option 8 to choose an interface, or option 7 for user-mode networking"
)

// bridgeIfaceAdvice is the tail every bridge validation error carries: the
// interfaces a bridge could use instead, and the controls that get the user
// out of it.
func bridgeIfaceAdvice(candidates []string, controls string) string {
	hint := "no host interface currently has a link"
	if len(candidates) > 0 {
		hint = "interfaces with a link: " + strings.Join(candidates, ", ")
	}
	return hint + "; " + controls
}

// validateBridgeIface turns an interface name plus its `ifconfig` status, the
// names of every interface on the host and the ones that can carry a bridge
// into an error the user can act on. status is the value
// parseDarwinIfaceStatus returned; an interface that reports no status at all
// cannot carry a bridge either. existing is what parseDarwinIfaceList read
// from `ifconfig -l`, and is empty when that call failed. controls is one of
// the two constants above.
//
// A bridge onto a dead interface is never what the caller wanted: the VM boots,
// the console works, and its DHCP requests reach the vmnet bridge and stop
// there, so it silently never gets an address (kairos-io/kairos#4431).
func validateBridgeIface(name, status string, existing, candidates []string, controls string) error {
	advice := bridgeIfaceAdvice(candidates, controls)

	// Every caller guards against this today, but they do it by standing in
	// the right place rather than by asking, and the message an empty name
	// produced named no interface and carried a double space.
	if name == "" {
		return fmt.Errorf("no bridge interface chosen (%s)", advice)
	}
	// A name that is not on the host is a typo, not a link problem: `ifconfig
	// en9` fails, so darwinIfaceStatus returns "" for it, which is
	// indistinguishable from an interface that genuinely reports no status
	// (kairos-io/kairos#4649).
	if len(existing) > 0 && !slices.Contains(existing, name) {
		return fmt.Errorf("no such interface %s on this host (%s)", name, advice)
	}
	// DetectBridgeIfaceCandidates already drops these, so an explicit
	// -bridge-if or a name typed into the config review is the only way one
	// reaches here. awdl0 is AirDrop and reports "active" on a real Mac, so
	// the status check below would wave it through.
	if isDarwinVirtualInterface(name) {
		return fmt.Errorf("bridge interface %s is a virtual device, and vmnet can only bridge onto a physical port (%s)", name, advice)
	}
	if status == "active" {
		return nil
	}
	detail := "reports no link status"
	if status != "" {
		detail = fmt.Sprintf("is %s", status)
	}
	return fmt.Errorf(
		"bridge interface %s %s, so the VM would boot with no network and no DHCP lease (%s)",
		name, detail, advice,
	)
}

// wifiBridgeWarning is the caveat printed when the bridge lands on a Wi-Fi
// radio. Bridging works often enough to be worth allowing and fails often
// enough to be worth saying out loud first.
func wifiBridgeWarning(name string) string {
	return fmt.Sprintf(
		"warning: %s is Wi-Fi. Access points routinely drop frames from a MAC other than the one that associated, so a bridged VM may never get a lease. Use -network user if it does not.",
		name,
	)
}
