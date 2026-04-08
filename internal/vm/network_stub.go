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

func CleanupLinuxBridge(_ *state.State) error {
	return nil
}

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
