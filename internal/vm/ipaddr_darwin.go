//go:build darwin

package vm

import (
	"context"
	"time"
)

// darwinLeaseFile is where Apple's DHCP server records its leases:
// bootpd.tproj/dhcpd.c defines DHCP_LEASES_FILE as exactly this path. It is
// one file for every vmnet client on the host, not one per interface, which
// is why defaultLeaseFile ignores the bridge name here.
//
// It is also vmnet-SHARED's database and nothing else: the bootpd behind it
// serves the shared subnet the framework hands out, and a bridged guest takes
// its lease from a server on the physical network that never writes here.
// There is no per-mode path to return, and no field for a bridged run to
// clear the way PrepareLinuxBridge clears DHCPLeaseFile on Linux -- which is
// why the mode gate that keeps a bridged run out of this file lives in
// Resolve, above both platforms, rather than in either of them.
const darwinLeaseFile = "/var/db/dhcpd_leases"

func defaultLeaseFile(_ string) string { return darwinLeaseFile }

func leaseLookup(path, mac string, now time.Time) string {
	return lookupLeaseFile(path, mac, now, parseDarwinLeases)
}

// arpInterfaceName is "" for every mode on macOS: there is no interface name
// here worth preferring an ARP entry on.
//
// The interface a vmnet guest is on is created by the framework, named
// bridge100 and upwards in creation order, and recorded nowhere this package
// can read -- state.Network.BridgeName on macOS holds the name the user asked
// for, not the name vmnet made. Passing that would prefer entries on an
// interface that does not exist, which the parsers treat as "no match on the
// named interface" and answer from the fallback, so it would be harmless and
// also pointless. "" says the same thing honestly, and gives the parsers
// their pre-existing first-match-wins behaviour.
func arpInterfaceName(_, _ string) string { return "" }

// arpLookup asks the host's ARP cache. -a lists every entry and -n keeps
// numeric addresses, so nothing here waits on a reverse DNS lookup -- which
// on a machine whose resolver is slow would otherwise cost seconds per tick.
func arpLookup(ctx context.Context, mac, iface string) string {
	return parseDarwinARP(hostCommandOutput(ctx, "arp", "-an"), mac, iface)
}
