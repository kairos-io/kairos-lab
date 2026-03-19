package vm

import (
	"reflect"
	"testing"
)

func TestParseIPv4Addrs(t *testing.T) {
	input := "2: enp0s31f6    inet 192.168.68.60/22 brd 192.168.71.255 scope global dynamic enp0s31f6\n2: enp0s31f6    inet 10.0.0.10/24 scope global secondary enp0s31f6\n"
	got := parseIPv4Addrs(input)
	want := []string{"10.0.0.10/24", "192.168.68.60/22"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIPv4Addrs() = %v, want %v", got, want)
	}
}

func TestRewriteRouteDevice(t *testing.T) {
	route := "default via 192.168.68.1 dev enp0s31f6 proto dhcp metric 100"
	got, ok := rewriteRouteDevice(route, "enp0s31f6", "kairoslab0")
	if !ok {
		t.Fatalf("expected route rewrite to succeed")
	}
	want := "default via 192.168.68.1 dev kairoslab0 proto dhcp metric 100"
	if got != want {
		t.Fatalf("rewriteRouteDevice() = %q, want %q", got, want)
	}
}

func TestRewriteRouteDeviceNoMatch(t *testing.T) {
	route := "default via 192.168.68.1 dev eth0"
	got, ok := rewriteRouteDevice(route, "enp0s31f6", "kairoslab0")
	if ok {
		t.Fatalf("expected no rewrite, got %q", got)
	}
	if got != route {
		t.Fatalf("expected unchanged route, got %q", got)
	}
}
