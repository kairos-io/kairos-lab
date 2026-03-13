package cleanup

import (
	"path/filepath"
	"testing"
)

func TestDependenciesToRemove(t *testing.T) {
	pre := []string{"qemu"}
	installed := []string{"qemu", "dnsmasq", "iproute2"}
	out := DependenciesToRemove(pre, installed)
	if len(out) != 2 {
		t.Fatalf("unexpected count: %d", len(out))
	}
}

func TestIsPathSafe(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a", "b")
	outside := filepath.Join(t.TempDir(), "x")
	if !IsPathSafe(inside, []string{root}) {
		t.Fatalf("expected inside path to be safe")
	}
	if IsPathSafe(outside, []string{root}) {
		t.Fatalf("expected outside path to be unsafe")
	}
}
