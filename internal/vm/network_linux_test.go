//go:build linux

package vm

import (
	"testing"
)

func TestParseNmCliColonField(t *testing.T) {
	const sampleDeviceShow = `
GENERAL.DEVICE:wlp9s0
GENERAL.STATE:connected
GENERAL.CONNECTION:My WPA
FOO:BAR
`

	v, ok := parseNmCliColonField(sampleDeviceShow, "general.connection")
	if !ok || v != "My WPA" {
		t.Fatalf("GENERAL.CONNECTION: got %q ok=%v", v, ok)
	}

	v, ok = parseNmCliColonField(`
connection.uuid:  abcdabcd-abcd-abcd-abcd-abcdabcdabcd  

`, "CONNECTION.UUID")
	if !ok || v != "abcdabcd-abcd-abcd-abcd-abcdabcdabcd" {
		t.Fatalf("connection.uuid: got %q ok=%v", v, ok)
	}
}

func TestParseNmcliUUIDNamePairs(t *testing.T) {
	in := `
b75cd23f-1abb-4762-b0ca-ff130fe2dd1e:kairoslab0-uplink
47843740-7891-407e-9ff1-5a4f729b5a1d:kairoslab0-uplink
a111aaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:wired ethernet
bbb:other
nosplit
`
	got := parseNmcliUUIDNamePairs(in, "kairoslab0-uplink")
	want := []string{"b75cd23f-1abb-4762-b0ca-ff130fe2dd1e", "47843740-7891-407e-9ff1-5a4f729b5a1d"}
	if len(got) != len(want) {
		t.Fatalf("len %d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}
