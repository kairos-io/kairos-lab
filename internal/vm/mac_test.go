package vm

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestMACForDiskShape(t *testing.T) {
	mac := MACForDisk("kairos-core-20250101-120000")
	if !strings.HasPrefix(mac, "52:54:00:") {
		t.Fatalf("MAC %q should carry the 52:54:00 QEMU prefix", mac)
	}
	octets := strings.Split(mac, ":")
	if len(octets) != 6 {
		t.Fatalf("MAC %q should have 6 octets, got %d", mac, len(octets))
	}
	for _, octet := range octets {
		if len(octet) != 2 {
			t.Errorf("octet %q of %q should be zero-padded to 2 digits", octet, mac)
		}
		if _, err := strconv.ParseUint(octet, 16, 8); err != nil {
			t.Errorf("octet %q of %q is not hex: %v", octet, mac, err)
		}
	}
}

func TestMACForDiskFirstOctetBits(t *testing.T) {
	// The bits are the property that matters, not the literal prefix: bit 0
	// clear means unicast, bit 1 set means locally administered (RFC 7042
	// section 2.1), which is what keeps the address off any vendor's range.
	first := strings.Split(MACForDisk("kairos-core-20250101-120000"), ":")[0]
	b, err := strconv.ParseUint(first, 16, 8)
	if err != nil {
		t.Fatalf("first octet %q is not hex: %v", first, err)
	}
	if b&0x01 != 0 {
		t.Errorf("first octet %#x has the multicast bit set, want unicast", b)
	}
	if b&0x02 != 0x02 {
		t.Errorf("first octet %#x is not locally administered", b)
	}
}

func TestMACForDiskIsStable(t *testing.T) {
	// The guest must present the same address after a restart or it loses its
	// DHCP lease.
	const disk = "kairos-core-20250101-120000"
	if first, second := MACForDisk(disk), MACForDisk(disk); first != second {
		t.Fatalf("MAC for %q changed between calls: %q then %q", disk, first, second)
	}
}

func TestMACForDiskIsDistinctPerDisk(t *testing.T) {
	a := MACForDisk("kairos-core-20250101-120000")
	b := MACForDisk("kairos-core-20250101-120001")
	if a == b {
		t.Fatalf("different disks share the MAC %q", a)
	}
}

func TestNormalizeMAC(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"linux zero-padded form", "52:54:00:12:34:56", "52:54:0:12:34:56", true},
		{"macos stripped form", "52:54:0:12:34:56", "52:54:0:12:34:56", true},
		{"all-zero octet keeps one digit", "0:0:0:0:0:0", "0:0:0:0:0:0", true},
		{"padded all-zero octet", "00:00:00:00:00:00", "0:0:0:0:0:0", true},
		{"leading zero stripped", "52:54:00:0a:04:0f", "52:54:0:a:4:f", true},
		{"uppercase lowercased", "52:54:00:AB:CD:EF", "52:54:0:ab:cd:ef", true},
		{"surrounding whitespace trimmed", "  52:54:00:12:34:56\n", "52:54:0:12:34:56", true},
		// Not a sensible NIC address, but a valid MAC shape: the normaliser
		// validates the shape and must not pass judgement on the value.
		{"broadcast address", "ff:ff:ff:ff:ff:ff", "ff:ff:ff:ff:ff:ff", true},
		{"empty string", "", "", false},
		{"not a mac", "not-a-mac", "", false},
		{"five octets", "52:54:00:12:34", "", false},
		{"seven octets", "52:54:00:12:34:56:78", "", false},
		{"three-digit octet", "52:54:000:12:34:56", "", false},
		{"empty octet", "52:54::12:34:56", "", false},
		{"non-hex octet", "52:54:00:12:34:zz", "", false},
		{"dash separated", "52-54-00-12-34-56", "", false},
		// A trailing colon splits into a seventh, empty octet.
		{"trailing colon", "52:54:00:12:34:56:", "", false},
		// "0x52" is four characters, so it is rejected on length before the
		// hex check ever sees the "x".
		{"0x-prefixed octet", "0x52:54:0:12:34:56", "", false},
	} {
		got, ok := NormalizeMAC(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: NormalizeMAC(%q) = (%q, %t), want (%q, %t)",
				tc.name, tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestNormalizeMACRejectedValuesAreIndistinguishable pins the hazard the bool
// exists for: an unset Disk.MAC and an unparseable lease MAC normalise to the
// SAME string, so a caller comparing values alone would attribute a DHCP lease
// to the wrong VM. Only the ok flag separates them.
func TestNormalizeMACRejectedValuesAreIndistinguishable(t *testing.T) {
	const garbage = "not-a-mac"
	emptyNorm, emptyOK := NormalizeMAC("")
	garbageNorm, garbageOK := NormalizeMAC(garbage)

	// Assert the trap first, so the ok assertions below cannot pass for the
	// wrong reason: if the rejected values ever stopped colliding, this test
	// would be guarding a hazard that no longer exists and must be rewritten
	// rather than quietly continuing to pass.
	if emptyNorm != garbageNorm {
		t.Fatalf("rejected inputs should be indistinguishable by value: "+
			"NormalizeMAC(%q) = %q but NormalizeMAC(%q) = %q", "", emptyNorm, garbage, garbageNorm)
	}
	if emptyOK {
		t.Errorf("NormalizeMAC(%q) = (%q, true), want ok=false for an unset MAC", "", emptyNorm)
	}
	if garbageOK {
		t.Errorf("NormalizeMAC(%q) = (%q, true), want ok=false for a malformed MAC", garbage, garbageNorm)
	}
	// The comparison a caller must never make, spelled out: equal values, and
	// the only thing that stops it being a match is that neither is usable.
	if emptyNorm == garbageNorm && emptyOK && garbageOK {
		t.Fatal("an unset MAC and a garbage MAC compared equal AND both reported usable")
	}
}

func TestNormalizeMACPaddedAndStrippedAgree(t *testing.T) {
	// The whole point of the normaliser: a lease file written by macOS and a
	// MAC written by us must compare equal.
	padded, paddedOK := NormalizeMAC("52:54:00:12:34:56")
	stripped, strippedOK := NormalizeMAC("52:54:0:12:34:56")
	if !paddedOK || !strippedOK {
		t.Fatalf("both forms should be usable: padded ok=%t, stripped ok=%t", paddedOK, strippedOK)
	}
	if padded == "" {
		t.Fatal("padded form should normalise to a non-empty value")
	}
	if padded != stripped {
		t.Fatalf("padded %q and stripped %q should normalise alike", padded, stripped)
	}
}

func TestNormalizeMACRoundTripsGeneratedMAC(t *testing.T) {
	mac := MACForDisk("kairos-core-20250101-120000")
	got, ok := NormalizeMAC(mac)
	if !ok {
		t.Fatalf("generated MAC %q should normalise", mac)
	}
	if got == "" {
		t.Fatalf("generated MAC %q normalised to an empty value", mac)
	}
}

func TestCanonicalMAC(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"padded form passes through", "52:54:00:12:34:56", "52:54:00:12:34:56", true},
		// The direction that matters: macOS prints MACs zero-stripped, and
		// this is where they get padded back into something a parser accepts.
		{"macos stripped form is padded", "52:54:0:12:34:56", "52:54:00:12:34:56", true},
		{"mixed padding is padded", "52:54:00:0a:4:f", "52:54:00:0a:04:0f", true},
		{"all-zero address", "0:0:0:0:0:0", "00:00:00:00:00:00", true},
		{"uppercase lowercased", "52:54:00:AB:CD:EF", "52:54:00:ab:cd:ef", true},
		{"surrounding whitespace trimmed", "  52:54:00:12:34:56\n", "52:54:00:12:34:56", true},
		{"broadcast address", "ff:ff:ff:ff:ff:ff", "ff:ff:ff:ff:ff:ff", true},
		{"empty string", "", "", false},
		{"whitespace only", "   ", "", false},
		{"not a mac", "not-a-mac", "", false},
		{"five octets", "52:54:00:12:34", "", false},
		{"seven octets", "52:54:00:12:34:56:78", "", false},
		{"three-digit octet", "52:54:000:12:34:56", "", false},
		{"empty octet", "52:54::12:34:56", "", false},
		{"non-hex octet", "52:54:00:12:34:zz", "", false},
		{"dash separated", "52-54-00-12-34-56", "", false},
		{"trailing colon", "52:54:00:12:34:56:", "", false},
		// A MAC with QEMU option syntax glued on: comma is the option
		// separator, so this must never reach a -device value.
		{"trailing qemu option", "52:54:00:12:34:56,romfile=/tmp/evil.rom", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CanonicalMAC(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("CanonicalMAC(%q) = (%q, %t), want (%q, %t)",
					tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestCanonicalMACAndNormalizeMACAgreeOnValidity is the property the shared
// parser exists for: the two formatters may disagree about the SHAPE of the
// result, never about which inputs are addresses. If they drift, a MAC that
// matches a DHCP lease could still be refused on its way to the QEMU command
// line, or the reverse.
func TestCanonicalMACAndNormalizeMACAgreeOnValidity(t *testing.T) {
	for _, in := range []string{
		"52:54:00:12:34:56",
		"52:54:0:12:34:56",
		"0:0:0:0:0:0",
		"00:00:00:00:00:00",
		"52:54:00:AB:CD:EF",
		"  52:54:00:12:34:56\n",
		"ff:ff:ff:ff:ff:ff",
		"",
		"   ",
		"not-a-mac",
		"52:54:00:12:34",
		"52:54:00:12:34:56:78",
		"52:54:000:12:34:56",
		"52:54::12:34:56",
		"52:54:00:12:34:zz",
		"52-54-00-12-34-56",
		"52:54:00:12:34:56:",
		"0x52:54:0:12:34:56",
		"52:54:00:12:34:56,romfile=/tmp/evil.rom",
	} {
		_, normOK := NormalizeMAC(in)
		_, canonOK := CanonicalMAC(in)
		if normOK != canonOK {
			t.Errorf("NormalizeMAC(%q) ok=%t but CanonicalMAC(%q) ok=%t: "+
				"the two must accept exactly the same addresses", in, normOK, in, canonOK)
		}
	}
}

// TestCanonicalMACIsTheParseableForm pins why the two forms are not
// interchangeable, so nobody "simplifies" CanonicalMAC into NormalizeMAC: the
// stripped comparison form is not a MAC a parser accepts, and the padded one
// is. net.ParseMAC stands in for QEMU's own parser here.
func TestCanonicalMACIsTheParseableForm(t *testing.T) {
	const stripped = "52:54:0:12:34:56"

	canonical, ok := CanonicalMAC(stripped)
	if !ok {
		t.Fatalf("CanonicalMAC(%q) rejected a MAC macOS prints every day", stripped)
	}
	if _, err := net.ParseMAC(canonical); err != nil {
		t.Errorf("net.ParseMAC(%q) = %v, the canonical form must be parseable", canonical, err)
	}

	// And the comparison form is not: this is the bug the split prevents.
	normalised, ok := NormalizeMAC("52:54:00:12:34:56")
	if !ok {
		t.Fatal("NormalizeMAC rejected a padded MAC")
	}
	if _, err := net.ParseMAC(normalised); err == nil {
		t.Errorf("net.ParseMAC(%q) unexpectedly succeeded: if the stripped form "+
			"became parseable, the reason CanonicalMAC exists needs rewriting, "+
			"not deleting", normalised)
	}
}

func TestCanonicalMACRoundTripsGeneratedMAC(t *testing.T) {
	mac := MACForDisk("kairos-core-20250101-120000")
	got, ok := CanonicalMAC(mac)
	if !ok {
		t.Fatalf("generated MAC %q should be canonicalisable", mac)
	}
	if got != mac {
		t.Errorf("CanonicalMAC(%q) = %q, a generated MAC is already canonical", mac, got)
	}
}
