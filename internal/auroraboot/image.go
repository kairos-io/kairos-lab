// Package auroraboot provides the `auroraboot` command that kairos-lab setup
// installs: a pinned container image, a shell shim that runs it, and the
// helpers that find a container runtime and manage the image.
package auroraboot

import "regexp"

const (
	// ImageRepo is the AuroraBoot image repository.
	ImageRepo = "quay.io/kairos/auroraboot"

	// ImageTag is the exact multi-arch tag the shim runs. It is never
	// "latest" and never a floating minor such as "v0.27", so a setup today
	// and a setup next month run the same AuroraBoot.
	// renovate: datasource=docker depName=quay.io/kairos/auroraboot
	ImageTag = "v0.28.0"
)

// ImageRef is the full reference setup pulls and the shim runs.
func ImageRef() string {
	return ImageRepo + ":" + ImageTag
}

// imageRefPattern is the only shape of image reference kairos-lab will pull,
// run or remove. state.json is writable by anything running as the user, so a
// reference read back from it is checked against this before it reaches a
// command line.
var imageRefPattern = regexp.MustCompile(`^quay\.io/kairos/auroraboot:[A-Za-z0-9._-]+$`)

// ValidImageRef reports whether ref is an AuroraBoot image reference with a
// tag. The check is anchored, so a trailing newline does not pass.
func ValidImageRef(ref string) bool {
	return imageRefPattern.MatchString(ref)
}
