//go:build darwin

package vm

import "os/exec"

func darwinRun(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func darwinIfaceStatus(iface string) string {
	return parseDarwinIfaceStatus(darwinRun("ifconfig", iface))
}

// DetectBridgeIfaceCandidates returns the physical interfaces that currently
// have a link, the one holding the default route first. vmnet-bridged can only
// bridge onto a physical interface, so tunnels, bridges and the AirDrop
// devices are filtered out.
func DetectBridgeIfaceCandidates() []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(iface string) {
		if iface == "" || seen[iface] || isDarwinVirtualInterface(iface) {
			return
		}
		if darwinIfaceStatus(iface) != "active" {
			return
		}
		seen[iface] = true
		candidates = append(candidates, iface)
	}

	add(parseDarwinDefaultRouteIface(darwinRun("route", "-n", "get", "default")))
	for _, iface := range parseDarwinIfaceList(darwinRun("ifconfig", "-l")) {
		add(iface)
	}
	return candidates
}

// ValidateBridgeIface fails when the chosen interface has no link, naming the
// interfaces that do.
func ValidateBridgeIface(iface string) error {
	return validateBridgeIface(iface, darwinIfaceStatus(iface), DetectBridgeIfaceCandidates())
}

// IsWiFiIface reports whether the interface is a Wi-Fi radio.
func IsWiFiIface(iface string) bool {
	ports := parseDarwinHardwarePorts(darwinRun("networksetup", "-listallhardwareports"))
	return isWiFiPort(ports[iface])
}

// WiFiBridgeWarning returns the caveat to print for a Wi-Fi bridge interface.
func WiFiBridgeWarning(iface string) string { return wifiBridgeWarning(iface) }

// BridgeIfaceStatus returns the `ifconfig` link status of an interface, or ""
// when it reports none.
func BridgeIfaceStatus(iface string) string { return darwinIfaceStatus(iface) }
