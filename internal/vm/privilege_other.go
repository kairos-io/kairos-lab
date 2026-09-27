//go:build !darwin

package vm

// checkNetworkPrivilege is a no-op off macOS. Linux escalates one command at
// a time through the sudo helper in network_linux.go, where each failure is
// reported by the command that needed it, and QEMU itself is deliberately
// left unprivileged -- the tap is created owned by the invoking user's uid
// for exactly that reason. There is no up-front privilege this tool can
// usefully demand here, and demanding one would refuse runs that work.
func checkNetworkPrivilege(_ string) error { return nil }
