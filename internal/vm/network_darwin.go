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

// ValidateBridgeIface rejects a chosen interface that vmnet cannot bridge
// onto, or that has no link, naming the interfaces that can. The advice it
// carries is worded for a command line.
func ValidateBridgeIface(iface string) error {
	return validateBridgeIfaceOnHost(iface, FlagBridgeControls)
}

// ValidateReviewBridgeIface is ValidateBridgeIface for a name typed into the
// interactive config review, where -bridge-if and -network are no longer the
// controls the user can reach.
func ValidateReviewBridgeIface(iface string) error {
	return validateBridgeIfaceOnHost(iface, ReviewBridgeControls)
}

func validateBridgeIfaceOnHost(iface, controls string) error {
	return validateBridgeIface(
		iface,
		darwinIfaceStatus(iface),
		parseDarwinIfaceList(darwinRun("ifconfig", "-l")),
		DetectBridgeIfaceCandidates(),
		controls,
	)
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
