//go:build !linux

package vm

import "github.com/kairos-io/kairos-lab/internal/state"

func PrepareLinuxBridge(_ *state.State, _ string) error {
	return nil
}

func CleanupLinuxBridge(_ *state.State) error {
	return nil
}
