package iso

import (
	"strings"
	"testing"
)

// versionsFromAssets runs the real pipeline: it names the ISOs the way a
// release does, parses them, and returns the version list the picker prints.
// Going through ParseISOAssets is what pins the format K3sVersion actually
// holds, "k3sv1.35.8+k3s1" rather than the bare "v1.35.8+k3s1" the field
// comment shows.
func versionsFromAssets(t *testing.T, versions ...string) []string {
	t.Helper()
	release := &Release{TagName: "v4.3.0"}
	for _, v := range versions {
		release.Assets = append(release.Assets, Asset{
			Name: "kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-" + v + ".iso",
		})
	}
	options := ParseISOAssets(release)
	if len(options) != len(versions) {
		t.Fatalf("ParseISOAssets kept %d of %d assets: %v", len(options), len(versions), options)
	}
	return GetK3sVersions(FilterByFlavor(options, "standard"))
}

// The picker labels the first entry "(latest)" and offers the rest below it,
// so this order decides both what a user reads and which image "(latest)"
// names. Sorted as characters, v1.9.0 outranks v1.10.0.
func TestGetK3sVersionsOrdersTheMinorNumerically(t *testing.T) {
	got := versionsFromAssets(t, "k3sv1.9.0+k3s1", "k3sv1.10.0+k3s1", "k3sv1.35.8+k3s1")

	assertOrder(t, got, []string{"k3sv1.35.8+k3s1", "k3sv1.10.0+k3s1", "k3sv1.9.0+k3s1"})
}

func TestGetK3sVersionsOrdersThePatchNumerically(t *testing.T) {
	got := versionsFromAssets(t, "k3sv1.34.9+k3s1", "k3sv1.34.11+k3s1")

	assertOrder(t, got, []string{"k3sv1.34.11+k3s1", "k3sv1.34.9+k3s1"})
}

// The k3s build stamp is a number too, so +k3s10 is newer than +k3s2 even
// though it sorts below it as a string.
func TestGetK3sVersionsOrdersTheBuildStampNumerically(t *testing.T) {
	got := versionsFromAssets(t, "k3sv1.35.8+k3s2", "k3sv1.35.8+k3s10", "k3sv1.35.8+k3s1")

	assertOrder(t, got, []string{"k3sv1.35.8+k3s10", "k3sv1.35.8+k3s2", "k3sv1.35.8+k3s1"})
}

// The k0s names a release publishes carry a dotted build stamp, "+k0s.0".
// Nothing offers them yet, but the comparison has to read them rather than
// fall back to character order the moment something does.
func TestCompareReadsTheDottedK0sBuildStamp(t *testing.T) {
	if compareK8sVersions("k0sv1.36.4+k0s.0", "k0sv1.35.8+k0s.0") <= 0 {
		t.Error("v1.36.4 should be newer than v1.35.8")
	}
	if compareK8sVersions("k0sv1.35.8+k0s.10", "k0sv1.35.8+k0s.2") <= 0 {
		t.Error("+k0s.10 should be newer than +k0s.2")
	}
}

// A version this cannot read must not be able to take "(latest)" from one it
// can, and two unreadable names must still come back in a stable order.
func TestCompareKeepsAnUnreadableVersionLast(t *testing.T) {
	if compareK8sVersions("k3sv9.9.9+k3sbeta", "k3sv1.10.0+k3s1") >= 0 {
		t.Error("an unreadable version should rank below a readable one")
	}
	if compareK8sVersions("beta", "alpha") <= 0 {
		t.Error("two unreadable versions should fall back to character order")
	}
}

// Equal versions compare equal, so a duplicate can never flip the list.
func TestCompareTreatsTheSameVersionAsEqual(t *testing.T) {
	if got := compareK8sVersions("k3sv1.35.8+k3s1", "k3sv1.35.8+k3s1"); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestGetK3sVersionsDropsCoreAndDeduplicates(t *testing.T) {
	release := &Release{TagName: "v4.3.0", Assets: []Asset{
		{Name: "kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k3sv1.9.0+k3s1.iso"},
		{Name: "kairos-hadron-v0.5.1-standard-arm64-generic-v4.3.0-k3sv1.9.0+k3s1.iso"},
		{Name: "kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k3sv1.10.0+k3s1.iso"},
		{Name: "kairos-hadron-v0.5.1-core-amd64-generic-v4.3.0.iso"},
	}}

	got := GetK3sVersions(FilterByFlavor(ParseISOAssets(release), "standard"))

	assertOrder(t, got, []string{"k3sv1.10.0+k3s1", "k3sv1.9.0+k3s1"})
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Two distributions can publish the same Kubernetes version, and the numbers
// alone cannot separate them. They still have to come back in one fixed order,
// so a list holding both does not shuffle between runs.
func TestCompareSeparatesTwoDistributionsOfOneVersion(t *testing.T) {
	if compareK8sVersions("k3sv1.36.4+k3s1", "k0sv1.36.4+k0s.1") <= 0 {
		t.Error("versions equal by number should still order by name")
	}
	if compareK8sVersions("k0sv1.36.4+k0s.1", "k3sv1.36.4+k3s1") >= 0 {
		t.Error("the reverse pair should order the other way")
	}
}
