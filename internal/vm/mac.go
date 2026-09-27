package vm

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// MACForDisk derives the guest NIC address for a disk from its name.
//
// QEMU seeds every process with the same default address, so two VMs sharing a
// subnet collide and a DHCP lease cannot be attributed to one of them. Hashing
// the disk name gives each VM its own address while keeping it deterministic:
// the same disk must present the same MAC after a restart, or the DHCP server
// hands it a different lease and the recorded IP goes stale.
//
// The 52:54:00 prefix is the conventional QEMU/KVM one and has the bits we
// need in its first octet (RFC 7042 section 2.1): 0x52&0x01 == 0 marks the
// address unicast, and 0x52&0x02 == 0x02 marks it locally administered, so it
// can never clash with a vendor-assigned address.
func MACForDisk(diskName string) string {
	sum := sha256.Sum256([]byte(diskName))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", sum[0], sum[1], sum[2])
}

// parseMAC is the single definition of "a MAC address this package accepts":
// six colon-separated hex octets of one or two digits each, case-insensitive,
// with surrounding whitespace ignored. It returns the six parsed bytes and
// true, or the zero array and false for anything else -- including the empty
// string.
//
// NormalizeMAC and CanonicalMAC both build on this and differ ONLY in how they
// format the result. Keeping the octet loop here is the point: the two
// formatters must never disagree about which inputs are valid, or a MAC that
// compares fine against a DHCP lease could still be rejected on its way to the
// QEMU command line (or worse, the reverse).
func parseMAC(s string) ([6]byte, bool) {
	var octets [6]byte
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), ":")
	if len(parts) != 6 {
		return [6]byte{}, false
	}
	for i, part := range parts {
		if len(part) < 1 || len(part) > 2 {
			return [6]byte{}, false
		}
		var b byte
		for _, c := range part {
			switch {
			case c >= '0' && c <= '9':
				b = b<<4 | byte(c-'0')
			case c >= 'a' && c <= 'f':
				b = b<<4 | byte(c-'a'+10)
			default:
				return [6]byte{}, false
			}
		}
		octets[i] = b
	}
	return octets, true
}

// NormalizeMAC reduces a MAC to one comparable form: lowercase, colon
// separated, with leading zeroes stripped from each octet. It returns the
// normalised address and true on success, and ("", false) for every rejected
// input -- including the empty string -- where "rejected" means anything that
// is not six colon-separated hex octets.
//
// This form is for COMPARISON ONLY. It is not a MAC any parser has to accept:
// see CanonicalMAC for the padded form to hand to QEMU or net.ParseMAC.
//
// Callers MUST check the bool before comparing. The normalised value alone
// cannot tell "no MAC" from "unparseable MAC": both are "", so an unchecked
//
//	NormalizeMAC(disk.MAC) == NormalizeMAC(leaseMAC)
//
// is true when disk.MAC is unset (legitimate for a disk recorded before the
// field existed) and leaseMAC is garbage, which silently attributes a DHCP
// lease to the wrong VM. The bool is the "this is a usable MAC" signal; two
// invalid MACs always compare equal.
//
// Both sides of every comparison have to go through this, because the hosts
// disagree on padding. macOS strips leading zeroes per octet -- bootplib's
// host_identifier.c formats with %x, not %02x, so 52:54:00:12:34:56 appears in
// /var/db/dhcpd_leases as 52:54:0:12:34:56, and network_cmds' arp.c prints the
// same way. Linux dnsmasq and `ip neigh` zero-pad with %.2x. Comparing the two
// raw would silently never match.
func NormalizeMAC(s string) (string, bool) {
	octets, ok := parseMAC(s)
	if !ok {
		return "", false
	}
	// %x, not %02x: stripping the padding is the whole job of this form.
	return fmt.Sprintf("%x:%x:%x:%x:%x:%x",
		octets[0], octets[1], octets[2], octets[3], octets[4], octets[5]), true
}

// CanonicalMAC returns the zero-padded lowercase colon form of a MAC --
// 52:54:00:12:34:56 -- and true, or ("", false) for every input NormalizeMAC
// also rejects. Both are built on parseMAC, so they accept exactly the same
// set of addresses and differ only in the formatting of the result.
//
// Do not "simplify" this into a call to NormalizeMAC: the two forms are not
// interchangeable, and this one is the one an actual parser accepts.
// NormalizeMAC deliberately STRIPS leading zeroes so that a MAC we wrote
// compares equal to the same MAC read back off macOS, which prints its
// addresses with %x (Apple's bootplib/host_identifier.c for
// /var/db/dhcpd_leases, network_cmds/arp.c for the ARP cache). That stripped
// form is a comparison key, nothing more: Go's own net.ParseMAC rejects
// "52:54:0:12:34:56", and QEMU's -device mac= property wants the padded
// spelling too. So CanonicalMAC ACCEPTS the stripped input a macOS source
// hands us and RETURNS the padded form -- that direction, never the reverse.
func CanonicalMAC(s string) (string, bool) {
	octets, ok := parseMAC(s)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x",
		octets[0], octets[1], octets[2], octets[3], octets[4], octets[5]), true
}
