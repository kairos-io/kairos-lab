//go:build !linux

package vm

import (
	"fmt"

	"github.com/kairos-io/kairos-lab/internal/state"
)

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

func PrepareLinuxVirbrTap(_ *state.State) error {
	return fmt.Errorf("virbr networking is only supported on Linux")
}

func CleanupLinuxVirbrTap(_ *state.State) error {
	return nil
}

func HasUsableLinuxVirbr() bool {
	return false
}

func IfaceIsWLAN(_ string) bool {
	return false
}

func PreferWiredUplinkForBridge() (string, error) {
	return "", fmt.Errorf("bridged uplink selection is Linux-only")
}

func CanSuggestAutoBridged() bool {
	return false
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
