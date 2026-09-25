//go:build !darwin

package vm

// The macOS bridge lives on a vmnet interface picked from the host's own
// interfaces; on every other platform there is nothing to resolve, so these
// are no-ops. See network_darwin.go.

func DetectBridgeIfaceCandidates() []string { return nil }

func ValidateBridgeIface(_ string) error { return nil }

func ValidateReviewBridgeIface(_ string) error { return nil }

func IsWiFiIface(_ string) bool { return false }

func WiFiBridgeWarning(iface string) string { return wifiBridgeWarning(iface) }

func BridgeIfaceStatus(_ string) string { return "" }
