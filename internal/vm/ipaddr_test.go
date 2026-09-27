// Tests for the discovery sources in ipaddr.go: the order Resolve asks them
// in, the loop and cancellation behaviour of Poll, and the guest-agent
// exchange.
//
// There is no build tag here either. ipaddr.go is untagged, so everything it
// contains runs on both CI legs; what differs per platform is only WHICH
// parser leaseLookup and arpLookup reach for, and the helpers below render
// their fixtures in the format of the host the test is running on. The two
// exec/socket seams -- hostCommandOutput and qgaDial -- are swapped for
// in-process fakes, so no test here runs a subprocess, opens a real socket or
// reads a file outside its own t.TempDir.
package vm

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// testMAC is the canonical padded spelling, as it would be handed to QEMU.
// The fixtures below write it out in the form the platform's own tooling
// would use, which on macOS is the stripped one.
//
// leaseFileIP and not leaseIP: (IPLookup).leaseIP is a method in ipaddr.go,
// and a package-level const of the same name is legal but makes the two
// spellings mean different things inside this one file.
const (
	testMAC     = "52:54:00:12:34:56"
	leaseFileIP = "192.168.64.12"
	arpIP       = "192.168.64.13"
	agentIP     = "192.168.64.14"
	staleARPIP  = "10.9.9.9"
	otherMAC    = "52:54:00:ab:cd:ef"
)

// hostSourcesAvailable reports whether this platform has a lease file and an
// ARP command this package knows how to read. Both CI legs do; the skips
// below exist so that a build for some third GOOS -- where ipaddr_other.go
// answers "" for both -- reports honestly rather than failing.
func hostSourcesAvailable() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

// platformLeaseContent renders a LIVE lease for mac in the format THIS
// platform's leaseLookup parses. macOS strips the leading zeroes of each
// octet, as bootpd does, so the fixture exercises the normalisation rather
// than sidestepping it.
//
// The expiry is in the year 2100 in both formats, because these fixtures are
// read against the real clock through Resolve and a lease that has run out is
// no longer an answer.
func platformLeaseContent(mac, ip string) string {
	return platformLeaseContentExpiring(mac, ip, "4102444800", "0xf4865700")
}

// platformStaleLeaseContent renders the record the same lease leaves behind
// once it has run out. Neither server deletes such a record -- dnsmasq keeps
// the line until the address is reused and bootpd keeps the group -- so this
// is what the file holds for a disk that ran before and is running again, and
// MACForDisk gives that disk the same MAC both times.
func platformStaleLeaseContent(mac, ip string) string {
	return platformLeaseContentExpiring(mac, ip, "1716915600", "0x664f1234")
}

func platformLeaseContentExpiring(mac, ip, dnsmasqExpiry, bootpdExpiry string) string {
	switch runtime.GOOS {
	case "darwin":
		stripped := strings.ReplaceAll(mac, ":0", ":")
		return "{\n\tname=kairos\n\tip_address=" + ip +
			"\n\thw_address=1," + stripped +
			"\n\tlease=" + bootpdExpiry + "\n}\n"
	case "linux":
		return dnsmasqExpiry + " " + mac + " " + ip + " kairos *\n"
	}
	return ""
}

// platformARPOutput renders one neighbour entry in the format this platform's
// arpLookup parses.
func platformARPOutput(mac, ip string) string {
	switch runtime.GOOS {
	case "darwin":
		return "? (" + ip + ") at " + strings.ReplaceAll(mac, ":0", ":") + " on bridge100 ifscope [ethernet]\n"
	case "linux":
		return ip + " dev kairoslab0 lladdr " + mac + " REACHABLE\n"
	}
	return ""
}

// platformTwoInterfaceARP renders the same MAC cached twice: a stale entry on
// the host's uplink, printed FIRST, and the live one on this VM's bridge.
// That is what `start --network bridged` followed by `start --network shared`
// on one disk leaves behind, and the order is the kernel's rather than a
// recency, so nothing but the interface distinguishes them.
func platformTwoInterfaceARP(mac, staleIP, freshIP string) string {
	switch runtime.GOOS {
	case "darwin":
		stripped := strings.ReplaceAll(mac, ":0", ":")
		return "? (" + staleIP + ") at " + stripped + " on en0 ifscope [ethernet]\n" +
			"? (" + freshIP + ") at " + stripped + " on " + DefaultBridgeName + " ifscope [ethernet]\n"
	case "linux":
		return staleIP + " dev wlan0 lladdr " + mac + " STALE\n" +
			freshIP + " dev " + DefaultBridgeName + " lladdr " + mac + " REACHABLE\n"
	}
	return ""
}

// wantARPArgv is the command this platform's ARP source must run. Asserting
// it is what keeps a "harmless" edit from turning `ip neigh` into
// `ip neigh show dev <x>`, which drops the dev token from the output and
// moves every field the parser reads.
func wantARPArgv() string {
	switch runtime.GOOS {
	case "darwin":
		return "arp -an"
	case "linux":
		return "ip neigh"
	}
	return ""
}

func qgaReplyFor(mac, ip string) string {
	return `{"return":[` +
		`{"name":"lo","hardware-address":"00:00:00:00:00:00","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4","prefix":8}]},` +
		`{"name":"eth0","hardware-address":"` + mac + `","ip-addresses":[` +
		`{"ip-address":"fe80::5054:ff:fe12:3456","ip-address-type":"ipv6","prefix":64},` +
		`{"ip-address":"` + ip + `","ip-address-type":"ipv4","prefix":24}]}]}`
}

// fakeSources stands in for the two seams of ipaddr.go: the ARP subprocess
// and the guest-agent socket. It records that each was consulted, which is
// what the source-order tests assert -- "the lease file answered" is only
// half the claim, the other half is that nothing else was asked.
type fakeSources struct {
	mu       sync.Mutex
	arpCalls []string
	arpOut   string
	qgaDials int
	qgaReply string
	qgaErr   error

	// The lease file read is recorded rather than replaced by default: the
	// tests that write a real file into t.TempDir still read that file, and
	// what the record adds is the other half of the claim -- WHICH path was
	// opened, and whether one was opened at all. stubLease switches the read
	// itself out, which is the only way to exercise the platform default,
	// whose path is under /var and is not a test's to create.
	leaseReads []string
	leaseOut   string
	leaseStub  bool

	stop chan struct{}
}

func newFakeSources(t *testing.T) *fakeSources {
	t.Helper()
	f := &fakeSources{stop: make(chan struct{})}

	origHost, origDial, origTimeout, origLease := hostCommandOutput, qgaDial, qgaReadTimeout, leaseFileContent
	t.Cleanup(func() {
		close(f.stop)
		hostCommandOutput, qgaDial, qgaReadTimeout, leaseFileContent = origHost, origDial, origTimeout, origLease
	})

	leaseFileContent = func(path string) string {
		f.mu.Lock()
		f.leaseReads = append(f.leaseReads, path)
		out, stubbed := f.leaseOut, f.leaseStub
		f.mu.Unlock()
		if stubbed {
			return out
		}
		return origLease(path)
	}

	hostCommandOutput = func(_ context.Context, name string, args ...string) string {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.arpCalls = append(f.arpCalls, strings.Join(append([]string{name}, args...), " "))
		return f.arpOut
	}

	qgaDial = func(_ context.Context, _ string) (net.Conn, error) {
		f.mu.Lock()
		f.qgaDials++
		reply, err := f.qgaReply, f.qgaErr
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		client, server := net.Pipe()
		go f.serve(server, reply)
		go func() {
			<-f.stop
			_ = server.Close()
		}()
		return client, nil
	}
	return f
}

// serve is QEMU's side of the chardev. With reply set it answers one
// LF-terminated line; with reply empty it reads the request and then NEVER
// answers, which is precisely what a "wait=off" chardev does when the guest
// ships no qemu-guest-agent -- the write is accepted into a virtio-serial
// port that nothing on the other side is reading.
func (f *fakeSources) serve(server net.Conn, reply string) {
	defer func() { _ = server.Close() }()
	buf := make([]byte, 4096)
	if _, err := server.Read(buf); err != nil {
		return
	}
	if reply == "" {
		<-f.stop
		return
	}
	_, _ = server.Write([]byte(reply + "\n"))
}

func (f *fakeSources) setARP(out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arpOut = out
}

func (f *fakeSources) setQGA(reply string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qgaReply = reply
}

func (f *fakeSources) setQGADialError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qgaErr = err
}

// stubLease answers every lease-file read with content, whatever the path.
func (f *fakeSources) stubLease(content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseOut, f.leaseStub = content, true
}

// leasePaths is every path the lease source opened, in order.
func (f *fakeSources) leasePaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.leaseReads...)
}

func (f *fakeSources) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.arpCalls...), f.qgaDials
}

// writeLease puts a lease file for mac in a temp dir and returns its path.
func writeLease(t *testing.T, mac, ip string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcp.leases")
	if err := os.WriteFile(path, []byte(platformLeaseContent(mac, ip)), 0o644); err != nil {
		t.Fatalf("write lease file: %v", err)
	}
	return path
}

// writeStaleLease puts the record of a lease that has already run out in a
// temp dir and returns its path.
func writeStaleLease(t *testing.T, mac, ip string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dhcp.leases")
	if err := os.WriteFile(path, []byte(platformStaleLeaseContent(mac, ip)), 0o644); err != nil {
		t.Fatalf("write stale lease file: %v", err)
	}
	return path
}

// missingLease is a path in a fresh temp dir that deliberately does not
// exist: the ordinary state of a lease file before the first client has
// leased anything.
func missingLease(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "nothing-here.leases")
}

func TestResolvePrefersTheLeaseFile(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	// Both later sources are armed with DIFFERENT addresses, so the test
	// fails loudly rather than coincidentally if the order is ever inverted.
	f.setARP(platformARPOutput(testMAC, arpIP))
	f.setQGA(qgaReplyFor(testMAC, agentIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeLease(t, testMAC, leaseFileIP), QGASocketPath: "/fake/qga.sock"}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != leaseFileIP || res.Source != IPSourceDHCPLease {
		t.Fatalf("Resolve = %+v, %v; want %s from %s", res, ok, leaseFileIP, IPSourceDHCPLease)
	}
	arp, qga := f.snapshot()
	if len(arp) != 0 {
		t.Errorf("ARP was consulted despite a lease file answer: %v", arp)
	}
	if qga != 0 {
		t.Errorf("the guest agent was dialled %d times despite a lease file answer", qga)
	}
}

func TestResolveFallsBackToARP(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no ARP source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))
	f.setQGA(qgaReplyFor(testMAC, agentIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t), QGASocketPath: "/fake/qga.sock"}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != arpIP || res.Source != IPSourceARP {
		t.Fatalf("Resolve = %+v, %v; want %s from %s", res, ok, arpIP, IPSourceARP)
	}
	arp, qga := f.snapshot()
	if len(arp) != 1 || arp[0] != wantARPArgv() {
		t.Errorf("ARP commands = %v, want exactly [%q]", arp, wantARPArgv())
	}
	if qga != 0 {
		t.Errorf("the guest agent was dialled %d times despite an ARP answer", qga)
	}
}

func TestResolveFallsBackToTheGuestAgentLast(t *testing.T) {
	f := newFakeSources(t)
	f.setARP("")
	f.setQGA(qgaReplyFor(testMAC, agentIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t), QGASocketPath: "/fake/qga.sock"}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != agentIP || res.Source != IPSourceGuestAgent {
		t.Fatalf("Resolve = %+v, %v; want %s from %s", res, ok, agentIP, IPSourceGuestAgent)
	}
	arp, qga := f.snapshot()
	if hostSourcesAvailable() && len(arp) != 1 {
		t.Errorf("ARP commands = %v, want the one probe that came up empty", arp)
	}
	if qga != 1 {
		t.Errorf("the guest agent was dialled %d times, want 1", qga)
	}
}

func TestResolveReturnsFalseWhenNothingAnswers(t *testing.T) {
	f := newFakeSources(t)
	f.setARP("")
	f.setQGA(`{"return":[]}`)

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t), QGASocketPath: "/fake/qga.sock"}
	if res, ok := l.Resolve(context.Background()); ok {
		t.Fatalf("Resolve = %+v, true; want no answer", res)
	}
}

// A lease for a DIFFERENT VM in the same file must not be mistaken for ours;
// this is the case the per-disk MAC exists to make decidable.
func TestResolveIgnoresAnotherVMsLease(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP("")
	f.setQGA(`{"return":[]}`)

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeLease(t, otherMAC, leaseFileIP), QGASocketPath: "/fake/qga.sock"}
	if res, ok := l.Resolve(context.Background()); ok {
		t.Fatalf("Resolve = %+v, true; want no answer for a lease belonging to %s", res, otherMAC)
	}
}

func TestResolveRejectsAnUnusableMAC(t *testing.T) {
	for _, mac := range []string{"", "not-a-mac", "52:54:00:12:34", "52:54:00:12:34:56:78"} {
		t.Run("mac="+mac, func(t *testing.T) {
			f := newFakeSources(t)
			f.setARP(platformARPOutput(testMAC, arpIP))
			f.setQGA(qgaReplyFor(testMAC, agentIP))

			l := IPLookup{MAC: mac, Mode: "shared", LeaseFile: writeLease(t, testMAC, leaseFileIP), QGASocketPath: "/fake/qga.sock"}
			if res, ok := l.Resolve(context.Background()); ok {
				t.Fatalf("Resolve with MAC %q = %+v, true; want no answer", mac, res)
			}
			// The point is not only the answer but the cost: with no usable
			// MAC there is nothing any source could be matched against, so
			// none of them may be probed at all.
			arp, qga := f.snapshot()
			if len(arp) != 0 || qga != 0 {
				t.Errorf("probed the host with an unusable MAC: arp=%v qga dials=%d", arp, qga)
			}
		})
	}
}

// user mode is QEMU's own SLIRP stack: its DHCP server is inside the QEMU
// process and no guest frame reaches the host, so neither host source can
// ever answer and neither should be run. The guest agent still is, because it
// reports what the guest itself sees.
func TestResolveSkipsHostSourcesInUserMode(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no host sources on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))
	f.setQGA(qgaReplyFor(testMAC, agentIP))

	l := IPLookup{MAC: testMAC, Mode: "user", LeaseFile: writeLease(t, testMAC, leaseFileIP), QGASocketPath: "/fake/qga.sock"}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != agentIP || res.Source != IPSourceGuestAgent {
		t.Fatalf("Resolve in user mode = %+v, %v; want %s from %s", res, ok, agentIP, IPSourceGuestAgent)
	}
	if arp, _ := f.snapshot(); len(arp) != 0 {
		t.Errorf("ARP was consulted in user mode: %v", arp)
	}
}

func TestResolveStopsOnAnAlreadyCancelledContext(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no host sources on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeLease(t, testMAC, leaseFileIP), QGASocketPath: "/fake/qga.sock"}
	if res, ok := l.Resolve(ctx); ok {
		t.Fatalf("Resolve on a cancelled context = %+v, true; want no answer", res)
	}
	if arp, qga := f.snapshot(); len(arp) != 0 || qga != 0 {
		t.Errorf("probed the host on a cancelled context: arp=%v qga dials=%d", arp, qga)
	}
}

func TestDefaultLeaseFile(t *testing.T) {
	got := defaultLeaseFile(DefaultBridgeName)
	var want string
	switch runtime.GOOS {
	case "darwin":
		// bootpd.tproj/dhcpd.c: DHCP_LEASES_FILE. One file for every vmnet
		// client, so the bridge name has no part in it.
		want = "/var/db/dhcpd_leases"
		if other := defaultLeaseFile("something-else"); other != want {
			t.Errorf("defaultLeaseFile(%q) = %q, want the same %q", "something-else", other, want)
		}
	case "linux":
		want = "/var/lib/NetworkManager/dnsmasq-" + DefaultBridgeName + ".leases"
		if empty := defaultLeaseFile(""); empty != "" {
			t.Errorf("defaultLeaseFile(\"\") = %q, want empty", empty)
		}
	}
	if got != want {
		t.Errorf("defaultLeaseFile(%q) on %s = %q, want %q", DefaultBridgeName, runtime.GOOS, got, want)
	}
}

// Every way a lease file read can fail is the same silent "": there is
// nothing here a user did wrong and nothing to report, only a next source to
// try.
func TestLeaseLookupToleratesAnUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		path string
	}{
		{"file that does not exist yet", filepath.Join(dir, "nothing-here.leases")},
		{"empty path", ""},
		{"a directory in place of the file", dir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := leaseLookup(tc.path, testMAC, testLeaseNow); got != "" {
				t.Errorf("leaseLookup(%q) = %q, want empty", tc.path, got)
			}
		})
	}
}

func TestParseQGAInterfaces(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		mac     string
		want    string
	}{
		{
			"ordinary response",
			qgaReplyFor(testMAC, agentIP),
			testMAC,
			agentIP,
		},
		{
			"interface with no hardware-address at all",
			`{"return":[{"name":"eth0","ip-addresses":[{"ip-address":"192.168.64.99","ip-address-type":"ipv4","prefix":24}]}]}`,
			testMAC,
			"",
		},
		{
			"interface with no ip-addresses yet",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:12:34:56"}]}`,
			testMAC,
			"",
		},
		{
			"interface with only an ipv6 address",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:12:34:56","ip-addresses":[{"ip-address":"fe80::5054:ff:fe12:3456","ip-address-type":"ipv6","prefix":64}]}]}`,
			testMAC,
			"",
		},
		{
			"loopback address on the matching interface is skipped",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:12:34:56","ip-addresses":[{"ip-address":"127.0.0.1","ip-address-type":"ipv4","prefix":8},{"ip-address":"192.168.64.14","ip-address-type":"ipv4","prefix":24}]}]}`,
			testMAC,
			agentIP,
		},
		{
			"the unconfigured 0.0.0.0 of a guest still waiting for a lease",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:12:34:56","ip-addresses":[{"ip-address":"0.0.0.0","ip-address-type":"ipv4","prefix":0}]}]}`,
			testMAC,
			"",
		},
		{
			"the second interface is the match",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:ab:cd:ef","ip-addresses":[{"ip-address":"10.0.0.9","ip-address-type":"ipv4","prefix":24}]},{"name":"eth1","hardware-address":"52:54:00:12:34:56","ip-addresses":[{"ip-address":"192.168.64.14","ip-address-type":"ipv4","prefix":24}]}]}`,
			testMAC,
			agentIP,
		},
		{
			"a guest that reports its address with stripped octets",
			`{"return":[{"name":"eth0","hardware-address":"52:54:0:12:34:56","ip-addresses":[{"ip-address":"192.168.64.14","ip-address-type":"ipv4","prefix":24}]}]}`,
			testMAC,
			agentIP,
		},
		{
			"an ipv4 address mislabelled ipv6 is not taken",
			`{"return":[{"name":"eth0","hardware-address":"52:54:00:12:34:56","ip-addresses":[{"ip-address":"192.168.64.14","ip-address-type":"ipv6","prefix":24}]}]}`,
			testMAC,
			"",
		},
		{"command not found error envelope", `{"error":{"class":"CommandNotFound","desc":"the command guest-network-get-interfaces has not been found"}}`, testMAC, ""},
		{"empty return", `{"return":[]}`, testMAC, ""},
		{"malformed json", `{"return":[{"name":`, testMAC, ""},
		{"empty payload", "", testMAC, ""},
		{"empty needle", qgaReplyFor(testMAC, agentIP), "", ""},
		{"garbage needle", qgaReplyFor(testMAC, agentIP), "not-a-mac", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseQGAInterfaces(tc.payload, tc.mac); got != tc.want {
				t.Errorf("parseQGAInterfaces(..., %q) = %q, want %q", tc.mac, got, tc.want)
			}
		})
	}
}

func TestGuestAgentIPReadsOneLine(t *testing.T) {
	f := newFakeSources(t)
	f.setQGA(qgaReplyFor(testMAC, agentIP))
	l := IPLookup{MAC: testMAC, QGASocketPath: "/fake/qga.sock"}
	if got := l.guestAgentIP(context.Background()); got != agentIP {
		t.Fatalf("guestAgentIP = %q, want %q", got, agentIP)
	}
}

func TestGuestAgentIPWithoutASocketPath(t *testing.T) {
	f := newFakeSources(t)
	f.setQGA(qgaReplyFor(testMAC, agentIP))
	l := IPLookup{MAC: testMAC}
	if got := l.guestAgentIP(context.Background()); got != "" {
		t.Fatalf("guestAgentIP with no socket path = %q, want empty", got)
	}
	if _, qga := f.snapshot(); qga != 0 {
		t.Errorf("dialled %d times with no socket path, want 0", qga)
	}
}

// A root-owned socket on a macOS host where QEMU runs under sudo: the dial
// fails with EACCES and the source is simply silent.
func TestGuestAgentIPToleratesADialError(t *testing.T) {
	f := newFakeSources(t)
	f.setQGADialError(errors.New("dial unix /fake/qga.sock: connect: permission denied"))
	l := IPLookup{MAC: testMAC, QGASocketPath: "/fake/qga.sock"}
	if got := l.guestAgentIP(context.Background()); got != "" {
		t.Fatalf("guestAgentIP with a refused dial = %q, want empty", got)
	}
}

// The wait=off case, and the reason qgaReadTimeout exists. QEMU accepts the
// connection and the write even when the guest ships no agent, so nothing
// ever answers and no error is ever reported. The context here has no
// deadline and is never cancelled, which leaves the source's OWN deadline as
// the only thing that can end the read.
func TestGuestAgentIPHonoursItsOwnDeadline(t *testing.T) {
	f := newFakeSources(t)
	f.setQGA("")
	qgaReadTimeout = 100 * time.Millisecond

	l := IPLookup{MAC: testMAC, QGASocketPath: "/fake/qga.sock"}
	done := make(chan string, 1)
	go func() { done <- l.guestAgentIP(context.Background()) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("guestAgentIP = %q, want empty", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guestAgentIP never returned: a wait=off chardev with no agent behind it answers nothing, so without its own read deadline this source blocks for the life of the VM")
	}
}

func TestGuestAgentIPReturnsOnContextCancellation(t *testing.T) {
	f := newFakeSources(t)
	f.setQGA("")
	// Long enough that only the cancellation can end the read.
	qgaReadTimeout = 5 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := IPLookup{MAC: testMAC, QGASocketPath: "/fake/qga.sock"}
	done := make(chan string, 1)
	go func() { done <- l.guestAgentIP(ctx) }()
	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("guestAgentIP = %q, want empty", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guestAgentIP ignored its cancelled context")
	}
}

func TestPollReturnsAsSoonAsASourceAnswers(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	newFakeSources(t)
	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeLease(t, testMAC, leaseFileIP)}

	start := time.Now()
	// The interval is deliberately far longer than the margin: an answer that
	// is already there must not cost a tick.
	res, ok := l.Poll(context.Background(), 30*time.Second, 10*time.Second)
	elapsed := time.Since(start)
	if !ok || res.IP != leaseFileIP || res.Source != IPSourceDHCPLease {
		t.Fatalf("Poll = %+v, %v; want %s from %s", res, ok, leaseFileIP, IPSourceDHCPLease)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Poll took %s to return an answer that was already available", elapsed)
	}
}

// pollResult is what a Poll running in its own goroutine reports back.
//
// Every Poll test runs it that way, and the reason is FIX 6's: a Poll that
// never returns -- which is what dropping the ctx.Done() case from its select
// produces -- cannot be caught by a test that measures elapsed time AFTER the
// call, because the call is where the test stops. The package then dies on
// the go test timeout with no assertion message at all, and CI passes no
// -timeout, so that is ten silent minutes instead of a named failure.
//
// The outer guards below are deliberately enormous next to what is being
// measured. They are not the assertion; the elapsed checks are. They exist so
// that a hang ends as a sentence naming the mechanism.
type pollResult struct {
	res     IPResult
	ok      bool
	elapsed time.Duration
}

func pollInBackground(ctx context.Context, l IPLookup, timeout, interval time.Duration) <-chan pollResult {
	done := make(chan pollResult, 1)
	start := time.Now()
	go func() {
		res, ok := l.Poll(ctx, timeout, interval)
		done <- pollResult{res: res, ok: ok, elapsed: time.Since(start)}
	}()
	return done
}

func TestPollGivesUpAtItsTimeout(t *testing.T) {
	newFakeSources(t)
	// No QGA socket path, so the last source costs nothing and the loop is
	// purely about the timeout.
	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t)}

	done := pollInBackground(context.Background(), l, 150*time.Millisecond, 20*time.Millisecond)
	select {
	case got := <-done:
		if got.ok {
			t.Fatalf("Poll = %+v, true; want no answer", got.res)
		}
		if got.elapsed < 100*time.Millisecond {
			t.Errorf("Poll gave up after %s, well before its 150ms timeout", got.elapsed)
		}
		if got.elapsed > 5*time.Second {
			t.Errorf("Poll took %s to honour a 150ms timeout", got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Poll never returned, 10s into a 150ms timeout: the timeout is applied by cancelling the context, so a loop that does not select on ctx.Done() waits on its ticker forever")
	}
}

// M5 cancels this the moment QEMU exits. A poller that finished its interval
// first would still be resolving, and still writing into state, after the VM
// it describes had gone.
func TestPollReturnsPromptlyOnCancellation(t *testing.T) {
	newFakeSources(t)
	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(50*time.Millisecond, cancel)

	done := pollInBackground(ctx, l, 60*time.Second, 5*time.Second)
	select {
	case got := <-done:
		if got.ok {
			t.Fatalf("Poll = %+v, true; want no answer", got.res)
		}
		// Generous against the 5s interval and the 60s timeout it was given:
		// anything near either of those means the cancellation was not
		// noticed.
		if got.elapsed > 2*time.Second {
			t.Errorf("Poll returned %s after its context was cancelled, with a 5s interval and a 60s timeout", got.elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Poll never returned 10s after its context was cancelled: M5 cancels this the moment QEMU exits, and a loop that ignores ctx.Done() is still resolving, and still writing into state, after the VM it describes has gone")
	}
}

func TestPollRejectsAnUnusableMAC(t *testing.T) {
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))
	l := IPLookup{MAC: "", Mode: "shared", LeaseFile: missingLease(t), QGASocketPath: "/fake/qga.sock"}

	start := time.Now()
	if res, ok := l.Poll(context.Background(), 30*time.Second, time.Second); ok {
		t.Fatalf("Poll with no MAC = %+v, true; want no answer", res)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Poll with no MAC took %s; it has nothing to match and must say so at once", elapsed)
	}
	if arp, qga := f.snapshot(); len(arp) != 0 || qga != 0 {
		t.Errorf("polled the host with no MAC: arp=%v qga dials=%d", arp, qga)
	}
}

// ---------------------------------------------------------------------------
// The lease source is a shared-mode source.

// bridged mode's DHCP server is on the physical network and writes no file we
// can read. PrepareLinuxBridge clears state.Network.DHCPLeaseFile for exactly
// that reason, and this is the reader that field was cleared for: an empty
// LeaseFile must not be read as "use the platform default", because the
// default recomputes the very path that was cleared (Linux) or names
// vmnet-shared's own database (macOS). Either one answers with the address a
// PREVIOUS run of this same disk held -- MACForDisk is deterministic, so the
// stale record matches -- on the first tick, labelled as the most trustworthy
// source there is.
func TestResolveDoesNotReadAnyLeaseFileInBridgedMode(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no host sources on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	// Every lease file on this host, whatever its path, would answer with the
	// previous guest's address. Nothing may open one.
	f.stubLease(platformLeaseContent(testMAC, leaseFileIP))
	f.setARP(platformARPOutput(testMAC, arpIP))

	for _, tc := range []struct {
		name string
		l    IPLookup
	}{
		{
			"lease path cleared by PrepareLinuxBridge",
			IPLookup{MAC: testMAC, Mode: "bridged", LeaseFile: "", BridgeName: DefaultBridgeName},
		},
		{
			"lease path left in state by an earlier shared run",
			IPLookup{MAC: testMAC, Mode: "bridged", LeaseFile: writeLease(t, testMAC, leaseFileIP), BridgeName: DefaultBridgeName},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := tc.l.Resolve(context.Background())
			if !ok || res.IP != arpIP || res.Source != IPSourceARP {
				t.Fatalf("Resolve in bridged mode = %+v, %v; want %s from %s -- the lease file is a shared-mode source",
					res, ok, arpIP, IPSourceARP)
			}
		})
	}
	if paths := f.leasePaths(); len(paths) != 0 {
		t.Errorf("bridged mode opened %d lease file(s): %v; it has no DHCP server of its own and must consult none", len(paths), paths)
	}
}

// The other half of the gate: shared mode still reads it, and still first.
func TestResolveStillReadsTheLeaseFileInSharedMode(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeLease(t, testMAC, leaseFileIP), BridgeName: DefaultBridgeName}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != leaseFileIP || res.Source != IPSourceDHCPLease {
		t.Fatalf("Resolve in shared mode = %+v, %v; want %s from %s", res, ok, leaseFileIP, IPSourceDHCPLease)
	}
	if paths := f.leasePaths(); len(paths) != 1 || paths[0] != l.LeaseFile {
		t.Errorf("lease files read = %v, want exactly [%q]", paths, l.LeaseFile)
	}
}

// user mode reads no lease file either, and never did: QEMU's SLIRP server
// writes nothing to the host.
func TestResolveReadsNoLeaseFileInUserMode(t *testing.T) {
	f := newFakeSources(t)
	f.stubLease(platformLeaseContent(testMAC, leaseFileIP))
	f.setQGA(qgaReplyFor(testMAC, agentIP))

	l := IPLookup{MAC: testMAC, Mode: "user", LeaseFile: "/should/not/be/read.leases", BridgeName: DefaultBridgeName, QGASocketPath: "/fake/qga.sock"}
	if res, ok := l.Resolve(context.Background()); !ok || res.Source != IPSourceGuestAgent {
		t.Fatalf("Resolve in user mode = %+v, %v; want the guest agent's answer", res, ok)
	}
	if paths := f.leasePaths(); len(paths) != 0 {
		t.Errorf("user mode opened %d lease file(s): %v", len(paths), paths)
	}
}

// ---------------------------------------------------------------------------
// leaseIP: which path it chooses, and what it refuses to build one from.

// The default-path branch is the only lease path macOS has, and on Linux it
// is what a state file written before DHCPLeaseFile existed falls back to.
// The path itself is under /var and no test may create it, so the read is
// stubbed and the assertion is on the path that was opened.
func TestLeaseIPFallsBackToThePlatformDefault(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.stubLease(platformLeaseContent(testMAC, leaseFileIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: "", BridgeName: DefaultBridgeName}
	if got := l.leaseIP(testLeaseNow); got != leaseFileIP {
		t.Fatalf("leaseIP with no recorded path = %q, want %q from the platform default", got, leaseFileIP)
	}
	want := defaultLeaseFile(DefaultBridgeName)
	if paths := f.leasePaths(); len(paths) != 1 || paths[0] != want {
		t.Errorf("lease files read = %v, want exactly [%q]", paths, want)
	}
}

// The recorded path still wins over the default when there is one.
func TestLeaseIPPrefersTheRecordedPath(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	recorded := writeLease(t, testMAC, leaseFileIP)

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: recorded, BridgeName: DefaultBridgeName}
	if got := l.leaseIP(testLeaseNow); got != leaseFileIP {
		t.Fatalf("leaseIP = %q, want %q", got, leaseFileIP)
	}
	if paths := f.leasePaths(); len(paths) != 1 || paths[0] != recorded {
		t.Errorf("lease files read = %v, want exactly [%q]", paths, recorded)
	}
}

// A lease that has run out is not an answer, through the whole source and
// against the real clock, not only in the parser's own table.
func TestLeaseIPIgnoresAnExpiredLease(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no lease file source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformARPOutput(testMAC, arpIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: writeStaleLease(t, testMAC, leaseFileIP)}
	if got := l.leaseIP(time.Now()); got != "" {
		t.Fatalf("leaseIP over a lease that expired in 2024 = %q, want empty", got)
	}
	// And Resolve then moves on to the next source rather than reporting the
	// previous guest's address as this one's.
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != arpIP || res.Source != IPSourceARP {
		t.Fatalf("Resolve over an expired lease = %+v, %v; want %s from %s", res, ok, arpIP, IPSourceARP)
	}
}

// BridgeName comes out of state.json, and defaultLeaseFile turns it into a
// path. sharedLeaseFilePath's docstring says both of its consumers sit
// downstream of validateStoredInterfaceName; this is the third one, and the
// dots in a name like "../../../../../tmp/x/kairoslab0" are popped by
// filepath.Join's cleaning, which is what performs the escape.
func TestDefaultLeaseFileRejectsAnUnusableBridgeName(t *testing.T) {
	for _, name := range []string{
		"../../../../../tmp/x/kairoslab0",
		"..",
		".",
		"kairos lab0",
		"-kairoslab0",
		"kairoslab0\n",
		"averyveryverylongbridgename",
		"",
	} {
		t.Run(name, func(t *testing.T) {
			got := defaultLeaseFile(name)
			if runtime.GOOS != "linux" {
				// Only the Linux default is built out of this name at all;
				// macOS returns one fixed path and has nothing to validate.
				t.Skipf("defaultLeaseFile ignores the bridge name on %s (= %q)", runtime.GOOS, got)
			}
			if got != "" {
				t.Errorf("defaultLeaseFile(%q) = %q, want empty: a name this shape must never become a path", name, got)
			}
		})
	}
}

// And a rejected name stops the source before any file is opened.
func TestLeaseIPOpensNothingForAnUnusableBridgeName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the bridge name is not part of the lease path on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.stubLease(platformLeaseContent(testMAC, leaseFileIP))

	l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: "", BridgeName: "../../../../../tmp/x/kairoslab0"}
	if got := l.leaseIP(testLeaseNow); got != "" {
		t.Fatalf("leaseIP with a path-escaping bridge name = %q, want empty", got)
	}
	if paths := f.leasePaths(); len(paths) != 0 {
		t.Errorf("opened %v; a rejected name must not reach a file read at all", paths)
	}
}

// ---------------------------------------------------------------------------
// The ARP source knows which interface this VM is on.

func TestArpInterfaceName(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"shared", DefaultBridgeName},
		{"bridged", DefaultBridgeName},
		{"user", ""},
		{"", ""},
		{"something-else", ""},
	} {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			want := tc.want
			if runtime.GOOS != "linux" {
				// macOS names the vmnet interface itself (bridge100 and up)
				// and records that name nowhere, so there is nothing here
				// worth preferring; every other GOOS has no ARP source.
				want = ""
			}
			if got := arpInterfaceName(tc.mode, DefaultBridgeName); got != want {
				t.Errorf("arpInterfaceName(%q, %q) on %s = %q, want %q", tc.mode, DefaultBridgeName, runtime.GOOS, got, want)
			}
		})
	}
}

// The same MAC on two interfaces at once is what one disk started in both
// network modes leaves behind, and `ip neigh` prints in the kernel's hash
// order, so the stale uplink entry can come first. On a platform that knows
// which interface this VM is on, the entry there wins.
func TestResolvePrefersTheARPEntryOnThisVMsInterface(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no ARP source on %s", runtime.GOOS)
	}
	f := newFakeSources(t)
	f.setARP(platformTwoInterfaceARP(testMAC, staleARPIP, arpIP))

	l := IPLookup{MAC: testMAC, Mode: "bridged", BridgeName: DefaultBridgeName}
	want := arpIP
	if arpInterfaceName(l.Mode, l.BridgeName) == "" {
		// This platform has no trustworthy interface name to prefer, so the
		// first entry wins exactly as it did before the preference existed.
		want = staleARPIP
	}
	res, ok := l.Resolve(context.Background())
	if !ok || res.IP != want || res.Source != IPSourceARP {
		t.Fatalf("Resolve = %+v, %v; want %s from %s", res, ok, want, IPSourceARP)
	}
}

// ---------------------------------------------------------------------------
// Poll's interval, and the defaults that set the real cadence.

// Nothing in the tree calls Poll with a non-positive interval yet, so the
// guard that replaces it with the default is reached only from here -- and
// without it time.NewTicker PANICS, which is one character away (<= to <).
func TestPollDefaultsANonPositiveInterval(t *testing.T) {
	if !hostSourcesAvailable() {
		t.Skipf("no ARP source to count attempts with on %s", runtime.GOOS)
	}
	for _, interval := range []time.Duration{0, -5 * time.Millisecond} {
		t.Run(interval.String(), func(t *testing.T) {
			f := newFakeSources(t)
			f.setARP("")
			// No lease file that answers and no QGA socket, so each attempt
			// is exactly one ARP probe and the probe count is the tick count.
			l := IPLookup{MAC: testMAC, Mode: "shared", LeaseFile: missingLease(t)}

			done := make(chan pollResult, 1)
			panicked := make(chan any, 1)
			start := time.Now()
			go func() {
				defer func() {
					if r := recover(); r != nil {
						panicked <- r
					}
				}()
				res, ok := l.Poll(context.Background(), 200*time.Millisecond, interval)
				done <- pollResult{res: res, ok: ok, elapsed: time.Since(start)}
			}()

			select {
			case r := <-panicked:
				t.Fatalf("Poll(%s) panicked: %v -- a non-positive interval must be replaced by DefaultIPPollInterval before time.NewTicker sees it", interval, r)
			case got := <-done:
				if got.ok {
					t.Fatalf("Poll = %+v, true; want no answer", got.res)
				}
				if got.elapsed < 150*time.Millisecond {
					t.Errorf("Poll returned after %s, well before its 200ms timeout", got.elapsed)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Poll never returned 10s into a 200ms timeout")
			}

			// One attempt: the immediate one. A second would mean the
			// interval became something far shorter than the 1s default
			// rather than the default itself.
			if arp, _ := f.snapshot(); len(arp) != 1 {
				t.Errorf("Poll made %d attempts in 200ms with interval %s; with the default of %s it makes exactly the one immediate attempt",
					len(arp), interval, DefaultIPPollInterval)
			}
		})
	}
}

// The cadence arithmetic, pinned. Every tick that finds no address pays the
// guest agent's read timeout in full, because QEMU's wait=off chardev accepts
// the connection and answers nothing on a guest with no agent -- so a read
// timeout at or above the poll interval does not slow the poll a little, it
// roughly halves the number of attempts a 45s budget buys.
func TestPollDefaults(t *testing.T) {
	if qgaReadTimeout >= DefaultIPPollInterval {
		t.Errorf("qgaReadTimeout = %s with a %s poll interval: every tick pays this in full, so the real cadence is interval+timeout",
			qgaReadTimeout, DefaultIPPollInterval)
	}
	if DefaultIPPollTimeout <= DefaultIPPollInterval {
		t.Errorf("DefaultIPPollTimeout = %s, not more than one interval of %s", DefaultIPPollTimeout, DefaultIPPollInterval)
	}
	// This pins the value. That a start passes it -- rather than a budget of
	// its own -- is pinned next door, by internal/app's
	// TestStartResolvesTheAddressBesideTheVMAndLeavesItForStatus.
	if DefaultIPPollTimeout != 45*time.Second {
		t.Errorf("DefaultIPPollTimeout = %s, want the 45s budget the PRD sets", DefaultIPPollTimeout)
	}
}

// ---------------------------------------------------------------------------
// The guest-agent exchange: its deadline clamp and its response cap.

func TestQGADeadline(t *testing.T) {
	now := time.Unix(1716000000, 0)
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want time.Time
	}{
		{
			"no deadline on the context: one exchange from now",
			func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			now.Add(qgaReadTimeout),
		},
		{
			"the caller's deadline is sooner: it wins",
			func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), now.Add(qgaReadTimeout/4))
			},
			now.Add(qgaReadTimeout / 4),
		},
		{
			"the caller's deadline is later: one exchange still bounds this source",
			func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), now.Add(time.Hour))
			},
			now.Add(qgaReadTimeout),
		},
		{
			"a deadline already in the past is still the answer",
			func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), now.Add(-time.Hour))
			},
			now.Add(-time.Hour),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()
			if got := qgaDeadline(now, ctx); !got.Equal(tc.want) {
				t.Errorf("qgaDeadline = %s, want %s", got, tc.want)
			}
		})
	}
}

// qgaMaxResponse is what stops something that is not qemu-ga from growing
// this read without bound. qemu-ga terminates its response with LF; an
// endpoint that never sends one is answered by the cap and by nothing else,
// which is why the read timeout here is set far beyond the test's own guard.
func TestGuestAgentIPStopsReadingAtTheResponseCap(t *testing.T) {
	newFakeSources(t)
	qgaReadTimeout = time.Minute

	var mu sync.Mutex
	written := 0
	qgaDial = func(_ context.Context, _ string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer func() { _ = server.Close() }()
			if _, err := server.Read(make([]byte, 4096)); err != nil {
				return
			}
			chunk := make([]byte, 4096)
			for i := range chunk {
				chunk[i] = 'A' // never an LF
			}
			for {
				n, err := server.Write(chunk)
				mu.Lock()
				written += n
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		return client, nil
	}

	l := IPLookup{MAC: testMAC, QGASocketPath: "/fake/qga.sock"}
	done := make(chan string, 1)
	go func() { done <- l.guestAgentIP(context.Background()) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("guestAgentIP over a stream that is not JSON = %q, want empty", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("guestAgentIP never returned: the endpoint sends no LF and the read deadline is a minute away, so only qgaMaxResponse can end this")
	}

	mu.Lock()
	total := written
	mu.Unlock()
	if total < qgaMaxResponse {
		t.Errorf("the endpoint got to write only %d bytes; the cap is %d and nothing else should have ended the read", total, qgaMaxResponse)
	}
	// One chunk of slack: the cap is checked after each read, so the last
	// read may cross it.
	if total > qgaMaxResponse+2*4096 {
		t.Errorf("the endpoint wrote %d bytes into a read capped at %d", total, qgaMaxResponse)
	}
}
