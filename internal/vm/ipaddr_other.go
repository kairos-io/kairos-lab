//go:build !darwin && !linux

package vm

import (
	"context"
	"time"
)

// Neither host source exists off macOS and Linux: there is no lease file this
// package knows the shape of, and no ARP command whose output it can parse.
// Both answer "" rather than guessing, which leaves the guest agent -- the
// one source that is the same everywhere -- as the only one Resolve consults
// on such a host.
//
// The symbols exist at all so that ipaddr.go, which is untagged, compiles on
// every GOOS this Go toolchain supports rather than only on the two the
// project ships.

func defaultLeaseFile(_ string) string { return "" }

func leaseLookup(_, _ string, _ time.Time) string { return "" }

// arpInterfaceName has no ARP source to name an interface for.
func arpInterfaceName(_, _ string) string { return "" }

func arpLookup(_ context.Context, _, _ string) string { return "" }
