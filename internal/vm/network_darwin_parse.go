package vm

import (
	"fmt"
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

// validateBridgeIface turns an interface name plus its `ifconfig` status and
// the list of interfaces that do have a link into an error the user can act
// on. status is the value parseDarwinIfaceStatus returned; an interface that
// reports no status at all cannot carry a bridge either.
//
// A bridge onto a dead interface is never what the caller wanted: the VM boots,
// the console works, and its DHCP requests reach the vmnet bridge and stop
// there, so it silently never gets an address (kairos-io/kairos#4431).
func validateBridgeIface(name, status string, candidates []string) error {
	if status == "active" {
		return nil
	}
	detail := "reports no link status"
	if status != "" {
		detail = fmt.Sprintf("is %s", status)
	}
	hint := "no host interface currently has a link"
	if len(candidates) > 0 {
		hint = "interfaces with a link: " + strings.Join(candidates, ", ")
	}
	return fmt.Errorf(
		"bridge interface %s %s, so the VM would boot with no network and no DHCP lease (%s; use -bridge-if to pick one, or -network user for port-forwarded access)",
		name, detail, hint,
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
