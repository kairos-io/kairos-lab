package auroraboot

// ValidRuntime reports whether name is a container runtime kairos-lab
// supports. The name ends up as an executed binary, so anything read back from
// state.json passes through here first.
func ValidRuntime(name string) bool {
	return name == "docker" || name == "podman"
}
