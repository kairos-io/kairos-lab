//go:build linux

package vm

import (
	"context"
	"time"
)

// defaultLeaseFile is only a fallback. The path that matters is the one
// PrepareLinuxShared recorded in state.Network.DHCPLeaseFile at the moment it
// applied ipv4.method shared, because the file is named after the interface
// that RECEIVED the method rather than after the bridge -- see
// sharedLeaseFilePath, which is called here rather than rebuilt so the two
// can never disagree about the spelling of that path.
//
// bridgeName comes out of state.json, so it is checked before it becomes a
// path. sharedLeaseFilePath's own docstring says both of its consumers sit
// downstream of validateStoredInterfaceName; this function was a third one
// that did not, and "../../../../../tmp/x/kairoslab0" in that field produced
// a path outside NMSTATEDIR -- filepath.Join cleans the result, and cleaning
// is what performs the escape, because the literal "dnsmasq-.." component is
// popped by the ".." that follows it. A rejected name yields "", which
// leaseIP reads as "no lease path" and the whole source is simply silent, the
// same answer it gives for a file that is not there yet.
//
// The error is dropped on purpose: nothing in this source reports anything.
// Every way of failing to read a lease -- absent file, denied read, unusable
// name -- means the same thing to the caller, which is to try the next
// source, and a per-tick message about it would be printed 45 times.
func defaultLeaseFile(bridgeName string) string {
	if err := validateStoredInterfaceName("bridge name", bridgeName); err != nil {
		return ""
	}
	return sharedLeaseFilePath(bridgeName)
}

func leaseLookup(path, mac string, now time.Time) string {
	return lookupLeaseFile(path, mac, now, parseDnsmasqLeases)
}

// arpInterfaceName is the interface the ARP source prefers an entry on.
//
// On Linux both bridged modes get a name, and it is the same name: shared and
// bridged each build the bridge in state.Network.BridgeName and put the tap
// on it, and in bridged mode the host's uplink is enslaved to that bridge as
// well, so the host's own neighbour entries for the segment are learned on
// the bridge in either mode. That is the entry worth preferring, because the
// one it is being preferred OVER is the stale entry for the same MAC left on
// the physical uplink by an earlier run in the other mode.
//
// user mode gets "" because no frame from a SLIRP guest ever reaches the
// host's neighbour table, so Resolve does not run the ARP source there at
// all. Any unrecognised mode gets "" too: linuxNetworkPreflight refuses to
// prepare one, so a name in that field describes nothing.
//
// The name is not validated here, and does not need to be: the only thing
// done with it is a string comparison against a token of `ip neigh` output.
// It reaches no path and no argv -- defaultLeaseFile above is where the same
// field does reach a path, and that is where it is checked.
func arpInterfaceName(mode, bridgeName string) string {
	switch mode {
	case "shared", "bridged":
		return bridgeName
	}
	return ""
}

// arpLookup asks the kernel's neighbour table. No "show" and no device
// filter: the unfiltered form is cheap, and filtering by device is precisely
// what makes iproute2 drop the "dev <ifname>" token from its output and shift
// every following field. parseLinuxNeigh keys off the lladdr keyword and so
// survives either shape, but there is no reason to hand it the harder one --
// and the unfiltered output is what lets iface be a preference with a
// fallback rather than a filter with nothing behind it.
func arpLookup(ctx context.Context, mac, iface string) string {
	return parseLinuxNeigh(hostCommandOutput(ctx, "ip", "neigh"), mac, iface)
}
