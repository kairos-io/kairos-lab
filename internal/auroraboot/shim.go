package auroraboot

import (
	_ "embed"
	"fmt"
	"strings"
)

// ShimMarker is the line that identifies a shim kairos-lab wrote. Install and
// remove act only on a regular file that carries it on a line of its own.
const ShimMarker = "# kairos-lab-managed: auroraboot shim"

//go:embed shim.sh
var shimTemplate string

// RenderShim returns the shim script for runtime, imageRef and goos. Every
// input is validated first: each one is written into a script the user later
// executes, so none may carry anything but the value it is meant to.
func RenderShim(runtime, imageRef, goos string) ([]byte, error) {
	if !ValidRuntime(runtime) {
		return nil, fmt.Errorf("unsupported container runtime %q", runtime)
	}
	if !ValidImageRef(imageRef) {
		return nil, fmt.Errorf("unsupported image reference %q", imageRef)
	}
	if goos != "linux" && goos != "darwin" {
		return nil, fmt.Errorf("unsupported operating system %q", goos)
	}
	out := strings.NewReplacer(
		"@@RUNTIME@@", runtime,
		"@@IMAGE@@", imageRef,
		"@@OS@@", goos,
	).Replace(shimTemplate)
	return []byte(out), nil
}
