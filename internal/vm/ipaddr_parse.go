package vm

import (
	"net"
	"strconv"
	"strings"
	"time"
)

// The parsing in this file is deliberately kept apart from the exec calls and
// the file reads in ipaddr.go and its platform files, exactly as
// network_darwin_parse.go is kept apart from network_darwin.go. Every
// function here takes a string and returns a string, so the whole address
// discovery surface is exercised on any host and on BOTH CI legs: the macOS
// formats are parsed under test on Linux and the Linux formats under test on
// macOS. Nothing here opens a file, runs a command or looks at runtime.GOOS.
//
// All four parsers obey the same three rules, and each of the three is a bug
// that was available to write:
//
//   - The needle MAC goes through NormalizeMAC ONCE, up front, and a !ok
//     needle returns "" before a single candidate is examined. NormalizeMAC
//     answers ("", false) for the empty string and for garbage alike, so
//     comparing normalised values without consulting the bool makes an unset
//     MAC equal to every unparseable candidate -- and a VM recorded before
//     the MAC field existed would then adopt the address of the first
//     malformed line in the file.
//   - Every candidate goes through NormalizeMAC too, because the two hosts
//     disagree about padding. macOS strips leading zeroes per octet
//     (bootplib/host_identifier.c and network_cmds/arp.c both format with %x),
//     so the NIC we hand QEMU as 52:54:00:12:34:56 comes back as
//     52:54:0:12:34:56; dnsmasq and `ip neigh` zero-pad with %.2x. A raw
//     comparison silently never matches on one of the two platforms.
//   - Every address goes through usableIPv4. All four sources carry IPv6 as
//     well, and an IPv6 address returned as "the VM's IP" is not a near miss:
//     it is recorded in state.json and printed as the address to ssh to.
//
// None of them may panic on ragged input. A lease file is read while its
// writer is appending to it, and an ARP table is whatever the host prints.
//
// Two further rules were added after the first version of this file, and both
// exist because a source that is merely OLD answers exactly like a source that
// is right:
//
//   - The two lease parsers take a now and honour the expiry their format
//     records. Neither DHCP server deletes an expired record: dnsmasq keeps
//     the line until the address is handed to somebody else, and bootpd keeps
//     the group until the entry is reused, so a file that names this MAC is
//     not on its own evidence that the address is still this guest's. The
//     clock is a PARAMETER and never time.Now(): everything in this file is a
//     pure function of its arguments, which is what lets both platforms'
//     formats be tested on either CI leg.
//   - The two ARP parsers take an iface and PREFER an entry on it. The
//     neighbour tables carry one entry per (address, interface) pair, so the
//     same MAC can legitimately appear twice -- once on the bridge this VM is
//     on and once, left over from an earlier run in another mode, on the
//     host's uplink -- and the order the kernel prints them in is a hash
//     order, not a recency order. The preference is soft on purpose; see
//     parseDarwinARP for why a hard filter would be worse than no filter.

// usableIPv4 returns the dotted-quad form of s when s is an IPv4 address a
// guest could actually be reached at, and "" for everything else.
//
// It is the single place the address rule lives, so the four parsers and the
// guest-agent source cannot drift apart about what counts as an answer.
//
// The To4 call is the IPv4 rule itself: net.ParseIP happily accepts
// "fd00::5", and every one of these sources carries IPv6 alongside IPv4. It
// also folds an IPv4-mapped form like "::ffff:192.168.64.12" down to the
// dotted quad a user can type.
//
// The unicast rule is the second half. IsGlobalUnicast rejects 0.0.0.0,
// 127.0.0.0/8, the 224.0.0.0/4 multicast range and 255.255.255.255 in one
// call, and all four are things these sources really do carry: an ARP table
// holds an entry for 224.0.0.251 (mDNS) on any mac, with a real link-layer
// address beside it. Link-local unicast is let through despite not being
// global, because a 169.254 address is what a guest that failed to get a
// lease genuinely has, and reporting it is more use than reporting nothing.
func usableIPv4(s string) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	if !v4.IsGlobalUnicast() && !v4.IsLinkLocalUnicast() {
		return ""
	}
	return v4.String()
}

// sameMAC reports whether candidate names the same NIC as needle, where
// needle has ALREADY been through NormalizeMAC and is known good. A candidate
// NormalizeMAC rejects is never a match -- that is the guard that keeps
// "(incomplete)", a DHCPv6 DUID and a torn line from matching anything.
func sameMAC(needle, candidate string) bool {
	got, ok := NormalizeMAC(candidate)
	return ok && got == needle
}

// ethernetHWAddress returns the address half of a bootpd hw_address value
// when the record describes an Ethernet client, and "" otherwise.
//
// The value is "<arp-hardware-type>,<address>": 1 is Ethernet (ARPHRD_ETHER),
// and ff is the marker bootpd writes for a DHCPv6 DUID, which is not a
// hardware address at all but an opaque client identifier that is merely
// spelled like one, colon-separated hex.
//
// The type prefix is the ONLY thing that says which of the two a value is.
// The server stores whatever bytes the client sent and does not validate
// their length, so a DUID of six octets -- which no well-formed DUID is, but
// any client on the segment can send -- is indistinguishable from a MAC once
// the prefix has been trimmed away, and a parser that blind-trimmed would
// hand a v6 record's address to whichever VM those six bytes happened to
// spell. Matching the type instead of stripping it is the whole point.
func ethernetHWAddress(value string) string {
	hwType, addr, found := strings.Cut(value, ",")
	if !found || strings.TrimSpace(hwType) != "1" {
		return ""
	}
	return strings.TrimSpace(addr)
}

// parseDarwinLeases finds the IPv4 address leased to mac in the contents of
// Apple's /var/db/dhcpd_leases, or "" when the file names no such client.
//
// The format is bootplib/NICache.c's PLCache_write: a "{" line, then
// tab-indented "<key>=<value>" lines, then a "}" line, one group per client.
// Three properties of that file decide the shape of this function:
//
//   - name= is OPTIONAL. bootpd's dhcpd.c writes it only "if (hostname_opt)",
//     so a guest that sends no hostname -- which is most of them during an
//     installer boot -- gets a record with no name line. Requiring one would
//     skip exactly the VMs we most want to find.
//   - identifier= is a separate line that is USUALLY but not always the same
//     as hw_address, so the match is made on hw_address alone. See
//     ethernetHWAddress for the DHCPv6 record where believing identifier is
//     actively wrong.
//   - The last record may have no closing brace, because this file is read
//     while bootpd is writing it. A record cut off that way still counts if
//     both fields we need arrived, which is why the match is attempted once
//     more after the loop rather than only on "}".
//
// lease= is read for its expiry and is the fourth property that shapes this.
// bootpd rewrites the whole cache rather than deleting single groups, so a
// client that went away months ago is still in the file with its old address;
// see bootpdLeaseExpired for what an absent or unreadable value means.
func parseDarwinLeases(content, mac string, now time.Time) string {
	needle, ok := NormalizeMAC(mac)
	if !ok {
		return ""
	}
	var ip, hw, lease string
	match := func() string {
		// sameMAC is the only guard needed: it rejects "" -- a record with no
		// hw_address line, or one whose type was not Ethernet -- exactly as
		// it rejects any other value NormalizeMAC will not take.
		if !sameMAC(needle, hw) {
			return ""
		}
		if bootpdLeaseExpired(lease, now) {
			return ""
		}
		return usableIPv4(ip)
	}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		switch line {
		case "{":
			ip, hw, lease = "", "", ""
			continue
		case "}":
			if got := match(); got != "" {
				return got
			}
			ip, hw, lease = "", "", ""
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ip_address":
			ip = strings.TrimSpace(value)
		case "hw_address":
			// "" for a DHCPv6 DUID record, which then matches nothing.
			hw = ethernetHWAddress(strings.TrimSpace(value))
		case "lease":
			lease = strings.TrimSpace(value)
		}
	}
	return match()
}

// parseDnsmasqLeases finds the IPv4 address leased to mac in the contents of
// the dnsmasq lease file NetworkManager writes for a shared connection
// (/var/lib/NetworkManager/dnsmasq-<iface>.leases), or "".
//
// The format is dnsmasq's lease_update_file: one space-separated line per
// lease, "<expiry> <mac> <ip> <hostname-or-*> <client-id-or-*>", with the MAC
// zero-padded. Two other line shapes live in the same file and neither is a
// v4 lease:
//
//   - a bare "duid <hex>" line, which carries the server's own DHCPv6 DUID
//     and has only two fields;
//   - IPv6 leases, "<expiry> <iaid> <v6addr> <hostname> <clid>", where the
//     second field is a numeric IAID rather than a MAC and the third is a v6
//     address. Those have the full five fields, so no field count
//     distinguishes them -- only the MAC and IPv4 checks do, which is why
//     both are applied to every line rather than only to lines that look
//     interesting.
//
// The length guard is therefore about indexing and nothing else: three is the
// number of fields this function READS, and it keeps the duid line from
// panicking a parser that reaches for fields[2].
//
// Field 0 is the expiry and it is honoured. dnsmasq's lease_update_file
// rewrites the file from its in-memory list, and a lease that ran out is
// removed from that list only when the address is given to somebody else or
// the server restarts, so an address a guest gave up weeks ago is still on
// disk under its MAC. Since MACForDisk derives the MAC from the disk name,
// the SAME disk started again matches its own stale line, and reporting that
// address as the running guest's is the failure this check exists for. See
// dnsmasqLeaseExpired for the two values that are not a time in the past.
func parseDnsmasqLeases(content, mac string, now time.Time) string {
	needle, ok := NormalizeMAC(mac)
	if !ok {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if !sameMAC(needle, fields[1]) {
			continue
		}
		if dnsmasqLeaseExpired(fields[0], now) {
			continue
		}
		if got := usableIPv4(fields[2]); got != "" {
			return got
		}
	}
	return ""
}

// dnsmasqLeaseExpired reports whether a dnsmasq lease whose expiry field is
// value had already run out at now.
//
// Three values are deliberately NOT expired:
//
//   - a zero now, which means "the caller has no clock to offer, do not check
//     the expiry at all". Every parser here is pure, and this is how a caller
//     that only wants the format parsed says so.
//   - an expiry of 0, which is dnsmasq's spelling of an INFINITE lease
//     (--dhcp-range ...,infinite writes 0 here, and lease_update_file writes
//     the same 0 for a lease with no expiry). Read as an epoch it is 1970 and
//     every such lease would be discarded, which is the opposite of what the
//     file says.
//   - anything that is not a decimal integer. The field is then not an expiry
//     this function understands, and a filter that cannot read a record's age
//     must not be the thing that discards it -- the MAC and IPv4 checks
//     remain what decides such a line, exactly as before this check existed.
func dnsmasqLeaseExpired(value string, now time.Time) bool {
	if now.IsZero() {
		return false
	}
	secs, err := strconv.ParseInt(value, 10, 64)
	if err != nil || secs == 0 {
		return false
	}
	return time.Unix(secs, 0).Before(now)
}

// bootpdLeaseExpired reports whether a bootpd lease record whose lease= value
// is value had already run out at now.
//
// The value is hex seconds since the epoch, written by bootplib as "0x%x"
// (dhcpd.c stores the lease expiration through PLCache; NICache.c prints it),
// so the 0x prefix is part of the real output and is trimmed here. A leading
// "0X" and a bare hex string are accepted too, because nothing downstream
// gains from being strict about a prefix.
//
// An absent lease= line means the check is SKIPPED and the record is kept, not
// that the record is dropped. The key is not guaranteed present -- a record
// bootpd wrote for a static entry has none, and a group being read while it is
// written may not have reached that line yet -- and dropping a record whose
// age is simply unknown would turn a lookup that works today into a silent
// empty answer. An unparseable or absurdly large value is treated the same
// way, for the reason given in dnsmasqLeaseExpired.
//
// Unlike dnsmasq there is no infinite-lease spelling to special-case here:
// bootpd writes an absolute expiration or no lease= line at all, so a 0 in
// this field really is 1970 and really is expired.
func bootpdLeaseExpired(value string, now time.Time) bool {
	if now.IsZero() || value == "" {
		return false
	}
	hex := value
	if len(hex) > 2 && hex[0] == '0' && (hex[1] == 'x' || hex[1] == 'X') {
		hex = hex[2:]
	}
	// 63 and not 64: the result is converted to a signed epoch, and a value
	// that would overflow into a negative time is rejected as unreadable
	// rather than silently becoming a moment long past.
	secs, err := strconv.ParseUint(hex, 16, 63)
	if err != nil {
		return false
	}
	return time.Unix(int64(secs), 0).Before(now)
}

// tokenValue returns the field that follows the first occurrence of key, or
// "" when key is absent or is the last field there is.
//
// Both ARP formats spell their optional attributes as a keyword followed by a
// value -- "on <ifname>" on macOS, "dev <ifname>" and "lladdr <mac>" on Linux
// -- and every one of them is omitted on some entries, so a field INDEX is
// wrong for the next line whatever it is right for. Reading by keyword is the
// only form that survives that, and it is the same rule parseLinuxNeigh
// already used for lladdr.
func tokenValue(fields []string, key string) string {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == key {
			return fields[i+1]
		}
	}
	return ""
}

// parseDarwinARP finds the IPv4 address `arp -an` reports for mac, preferring
// an entry on iface, or "".
//
// The layout is network_cmds' arp.c print_entry:
//
//	<host-or-?> (<ipv4>) at <mac|(incomplete)> [on <ifname>] [ifscope] [permanent] [published] [[ethernet]]
//
// Everything after the link-layer address is optional and varies by entry, so
// the parse keys off the literal " at " separator and takes the next
// whitespace token rather than counting fields from either end. An entry with
// no resolved address prints "(incomplete)" in that position; it needs no
// special case, because NormalizeMAC rejects it like any other non-MAC.
//
// The address is the last token before " at ", parenthesised by arp itself.
// Leading zeroes are stripped here too -- print_lladdr formats with %x -- so
// the same normalisation that covers the lease file covers this.
//
// iface is a PREFERENCE and not a filter, and the difference is the whole
// design of it. An entry on iface wins outright, wherever in the output it
// sits; an entry on any other interface, or on none, is remembered and
// returned only if iface named nothing. That covers the case this exists for
// -- the same MAC cached on the host's uplink from an earlier run in another
// mode, which `arp -an` may print before the bridge's entry because the order
// is the kernel's and not a recency -- without turning a working lookup into
// silence when the interface token is simply absent (the "on <ifname>" part
// is genuinely optional in this format, and the caller does not always know a
// name worth preferring). An empty iface therefore behaves exactly as this
// function did before the parameter existed: first usable match wins.
func parseDarwinARP(out, mac, iface string) string {
	needle, ok := NormalizeMAC(mac)
	if !ok {
		return ""
	}
	var elsewhere string
	for _, line := range strings.Split(out, "\n") {
		head, tail, found := strings.Cut(line, " at ")
		if !found {
			continue
		}
		lladdr := strings.Fields(tail)
		if len(lladdr) == 0 || !sameMAC(needle, lladdr[0]) {
			continue
		}
		addr := strings.Fields(head)
		if len(addr) == 0 {
			continue
		}
		got := usableIPv4(strings.Trim(addr[len(addr)-1], "()"))
		if got == "" {
			continue
		}
		if iface != "" && tokenValue(lladdr, "on") == iface {
			return got
		}
		if elsewhere == "" {
			elsewhere = got
		}
	}
	return elsewhere
}

// parseLinuxNeigh finds the IPv4 address `ip neigh` reports for mac, or "".
//
// The layout is iproute2's ipneigh.c print_neigh:
//
//	<dst> [dev <ifname>] [lladdr <mac>] [router] [proxy] ... <NUD_STATE> [proto <p>]
//
// Two of those brackets are why this keys off the lladdr KEYWORD instead of a
// field index. "dev <ifname>" is omitted whenever the command filtered by
// device, which shifts every later field left by two -- so an index that is
// right for `ip neigh` is wrong for `ip neigh show dev br0`, and a caller
// that adds a filter later would silently start reading the NUD state as a
// MAC. And FAILED and INCOMPLETE entries carry no lladdr at all, so there is
// no position to read in the first place.
//
// The address is always field 0, and it is genuinely an IPv6 address for
// every v6 neighbour on the bridge -- including one whose link-layer address
// is the very MAC being searched for, since a guest's fe80:: address is
// derived from that MAC. usableIPv4 is what keeps such an entry from being
// returned as the VM's address.
//
// dev is read for the same reason, and with the same softness, as "on" in
// parseDarwinARP: an entry on iface wins outright, an entry anywhere else is
// the fallback, and an empty iface leaves the original "first match wins"
// behaviour untouched. The token is absent from every line of a
// device-filtered `ip neigh show dev <x>`, which is exactly the shape where
// filtering hard would discard every candidate there is.
func parseLinuxNeigh(out, mac, iface string) string {
	needle, ok := NormalizeMAC(mac)
	if !ok {
		return ""
	}
	var elsewhere string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// From fields[1]: field 0 is the address, and only the attributes
		// after it are keyword/value pairs. One lladdr per entry, so the first
		// is the only one -- whether it matched or not, the line is finished.
		if !sameMAC(needle, tokenValue(fields[1:], "lladdr")) {
			continue
		}
		got := usableIPv4(fields[0])
		if got == "" {
			continue
		}
		if iface != "" && tokenValue(fields[1:], "dev") == iface {
			return got
		}
		if elsewhere == "" {
			elsewhere = got
		}
	}
	return elsewhere
}
