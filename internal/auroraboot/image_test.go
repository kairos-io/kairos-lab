package auroraboot

import (
	"regexp"
	"testing"
)

func TestImageRefIsPinned(t *testing.T) {
	if ImageTag == "latest" {
		t.Fatal("the image tag must not be latest")
	}
	if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(ImageTag) {
		t.Fatalf("tag %q is not an exact vX.Y.Z version", ImageTag)
	}
	if got, want := ImageRef(), ImageRepo+":"+ImageTag; got != want {
		t.Fatalf("ImageRef() = %q, want %q", got, want)
	}
	if !ValidImageRef(ImageRef()) {
		t.Fatalf("the pinned reference %q fails its own validator", ImageRef())
	}
}

func TestValidImageRef(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"pinned", "quay.io/kairos/auroraboot:v0.27.1", true},
		{"another repo", "quay.io/kairos/other:v1.0.0", false},
		{"another registry", "evil.example/kairos/auroraboot:v1.0.0", false},
		{"no tag", "quay.io/kairos/auroraboot", false},
		{"digest", "quay.io/kairos/auroraboot@sha256:abcd", false},
		{"trailing newline", "quay.io/kairos/auroraboot:v1\n", false},
		{"embedded newline", "quay.io/kairos/auroraboot:v1\nx", false},
		{"option lookalike", "quay.io/kairos/auroraboot:v1 --privileged", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		if got := ValidImageRef(tc.ref); got != tc.want {
			t.Errorf("%s: ValidImageRef(%q) = %v, want %v", tc.name, tc.ref, got, tc.want)
		}
	}
}
