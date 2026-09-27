//go:build !linux

package vm

import "github.com/kairos-io/kairos-lab/internal/state"

const (
	DefaultBridgeName = "kairoslab0"
	DefaultTapName    = "kairoslab-tap0"
)

func PrepareLinuxBridge(_ *state.State, _ string) error {
	return nil
}

func PrepareLinuxShared(_ *state.State, _ string) error {
	return nil
}

func CleanupLinuxBridge(_ *state.State) error {
	return nil
}

// IsLinuxBridge is a function here and a function on Linux too, where the
// swappable seam the tests need is an unexported var one level down. Keeping
// the exported identifier the same kind on both platforms is what stops
// `vm.IsLinuxBridge = f` from compiling on one GOOS and failing on the other.
func IsLinuxBridge(_ string) bool {
	return false
}

func HasStaleNetworkResources(_ *state.State) bool {
	return false
}

func CleanupStaleNetworkResources(_ *state.State) error {
	return nil
}

func DetectUplinkCandidates() []string {
	return nil
}
