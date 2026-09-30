package iso

import (
	"strings"
	"testing"
)

// v430Assets is the ISO half of the kairos-io/kairos v4.3.0 release payload,
// verbatim. It carries three k0s images that the tool used to drop.
var v430Assets = []string{
	"kairos-hadron-v0.5.1-core-amd64-generic-v4.3.0.iso",
	"kairos-hadron-v0.5.1-core-arm64-generic-v4.3.0.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k0sv1.34.11+k0s.0.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k0sv1.35.8+k0s.0.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k0sv1.36.4+k0s.0.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k3sv1.34.11+k3s1.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k3sv1.35.8+k3s1.iso",
	"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k3sv1.36.4+k3s1.iso",
	"kairos-hadron-v0.5.1-standard-arm64-generic-v4.3.0-k3sv1.34.11+k3s1.iso",
	"kairos-hadron-v0.5.1-standard-arm64-generic-v4.3.0-k3sv1.35.8+k3s1.iso",
	"kairos-hadron-v0.5.1-standard-arm64-generic-v4.3.0-k3sv1.36.4+k3s1.iso",
}

func releaseOf(names ...string) *Release {
	r := &Release{TagName: "v4.3.0"}
	for _, n := range names {
		r.Assets = append(r.Assets, Asset{
			Name:               n,
			BrowserDownloadURL: "https://example.invalid/" + n,
			Size:               1,
		})
	}
	return r
}

func TestParseISOAssetsKeepsEveryPublishedISO(t *testing.T) {
	opts := ParseISOAssets(releaseOf(v430Assets...))
	if len(opts) != len(v430Assets) {
		t.Fatalf("parsed %d of %d published ISOs", len(opts), len(v430Assets))
	}
	for _, name := range v430Assets {
		found := false
		for _, o := range opts {
			if o.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was dropped", name)
		}
	}
}

func TestParseISOAssetsReadsTheK0sDistroAndVersion(t *testing.T) {
	opts := ParseISOAssets(releaseOf("kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k0sv1.36.4+k0s.0.iso"))
	if len(opts) != 1 {
		t.Fatalf("got %d options, want 1", len(opts))
	}
	got := opts[0]
	if got.Flavor != "standard" || got.Arch != "amd64" {
		t.Errorf("flavor/arch = %q/%q, want standard/amd64", got.Flavor, got.Arch)
	}
	if got.K8sDistro != "k0s" {
		t.Errorf("distro = %q, want k0s", got.K8sDistro)
	}
	if got.K8sVersion != "v1.36.4+k0s.0" {
		t.Errorf("version = %q, want v1.36.4+k0s.0", got.K8sVersion)
	}
}

func TestParseISOAssetsReadsTheK3sDistroAndVersion(t *testing.T) {
	opts := ParseISOAssets(releaseOf("kairos-hadron-v0.5.1-standard-arm64-generic-v4.3.0-k3sv1.35.8+k3s1.iso"))
	if len(opts) != 1 {
		t.Fatalf("got %d options, want 1", len(opts))
	}
	if opts[0].K8sDistro != "k3s" || opts[0].K8sVersion != "v1.35.8+k3s1" {
		t.Errorf("got %q/%q, want k3s/v1.35.8+k3s1", opts[0].K8sDistro, opts[0].K8sVersion)
	}
}

func TestParseISOAssetsLeavesACoreImageWithNoKubernetes(t *testing.T) {
	opts := ParseISOAssets(releaseOf("kairos-hadron-v0.5.1-core-amd64-generic-v4.3.0.iso"))
	if len(opts) != 1 {
		t.Fatalf("got %d options, want 1", len(opts))
	}
	if opts[0].K8sDistro != "" || opts[0].K8sVersion != "" {
		t.Errorf("core image carries %q/%q, want both empty", opts[0].K8sDistro, opts[0].K8sVersion)
	}
}

func TestParseISOAssetsStillRejectsAnUnrelatedAsset(t *testing.T) {
	opts := ParseISOAssets(releaseOf(
		"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k9sv1.36.4+k9s1.iso",
		"some-other-project.iso",
		"kairos-hadron-v0.5.1-standard-amd64-generic-v4.3.0-k0sv1.36.4.iso",
	))
	if len(opts) != 0 {
		t.Fatalf("got %d options, want 0: %+v", len(opts), opts)
	}
}

func TestGetKubernetesOptionsListsBothDistrosWithK3sFirst(t *testing.T) {
	std := FilterByFlavor(FilterByArch(ParseISOAssets(releaseOf(v430Assets...)), "amd64"), "standard")
	got := GetKubernetesOptions(std)

	want := []KubernetesOption{
		{Distro: "k3s", Version: "v1.36.4+k3s1"},
		{Distro: "k3s", Version: "v1.35.8+k3s1"},
		{Distro: "k3s", Version: "v1.34.11+k3s1"},
		{Distro: "k0s", Version: "v1.36.4+k0s.0"},
		{Distro: "k0s", Version: "v1.35.8+k0s.0"},
		{Distro: "k0s", Version: "v1.34.11+k0s.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d options, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("option %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFindByKubernetesDistinguishesTheDistros(t *testing.T) {
	std := FilterByFlavor(FilterByArch(ParseISOAssets(releaseOf(v430Assets...)), "amd64"), "standard")

	k0s := FindByKubernetes(std, KubernetesOption{Distro: "k0s", Version: "v1.36.4+k0s.0"})
	if k0s == nil {
		t.Fatal("no ISO found for k0s v1.36.4+k0s.0")
	}
	if !strings.Contains(k0s.Name, "k0sv1.36.4+k0s.0") {
		t.Errorf("k0s selection resolved to %s", k0s.Name)
	}

	k3s := FindByKubernetes(std, KubernetesOption{Distro: "k3s", Version: "v1.36.4+k3s1"})
	if k3s == nil {
		t.Fatal("no ISO found for k3s v1.36.4+k3s1")
	}
	if !strings.Contains(k3s.Name, "k3sv1.36.4+k3s1") {
		t.Errorf("k3s selection resolved to %s", k3s.Name)
	}

	if FindByKubernetes(std, KubernetesOption{Distro: "k0s", Version: "v1.36.4+k3s1"}) != nil {
		t.Error("a version from one distro matched an image of the other")
	}
}

func TestKubernetesOptionLabelNamesTheDistro(t *testing.T) {
	got := KubernetesOption{Distro: "k0s", Version: "v1.36.4+k0s.0"}.Label()
	if got != "k0s v1.36.4+k0s.0" {
		t.Errorf("label = %q", got)
	}
}

func TestPrintKubernetesChoicesMarksTheLatestOfEachDistro(t *testing.T) {
	std := FilterByFlavor(FilterByArch(ParseISOAssets(releaseOf(v430Assets...)), "amd64"), "standard")

	var out strings.Builder
	printKubernetesChoices(&out, GetKubernetesOptions(std))

	want := "\nSelect Kubernetes version:\n" +
		"  [1] k3s v1.36.4+k3s1 (latest)\n" +
		"  [2] k3s v1.35.8+k3s1\n" +
		"  [3] k3s v1.34.11+k3s1\n" +
		"  [4] k0s v1.36.4+k0s.0 (latest)\n" +
		"  [5] k0s v1.35.8+k0s.0\n" +
		"  [6] k0s v1.34.11+k0s.0\n" +
		"Choice [1-6]: "
	if out.String() != want {
		t.Errorf("picker printed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestGetKubernetesOptionsListsEachVersionOnce(t *testing.T) {
	// Both architectures ship the same three k3s versions, so an unfiltered
	// list carries every one of them twice.
	all := FilterByFlavor(ParseISOAssets(releaseOf(v430Assets...)), "standard")
	got := GetKubernetesOptions(all)

	if len(got) != 6 {
		t.Fatalf("got %d options, want 6: %+v", len(got), got)
	}
	seen := make(map[KubernetesOption]int)
	for _, k := range got {
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("%s listed %d times", k.Label(), n)
		}
	}
}

// TestParseISOAssetsRejectsAMixedDistroStamp pins the agreement between the
// two halves of a standard image's name. `k3s` names the distribution and
// `+k0s.0` stamps the build, so the name contradicts itself; the pattern's
// two groups are independent and cannot catch that on their own.
//
// It matters because nothing downstream reads either half alone. The picker
// groups by distro, FindByK8sVersion looks the pair up and the dedupe keys on
// it, so an asset admitted here under "k3s" carrying a k0s version would
// offer a k0s image to a user who asked for k3s. The well-formed asset in the
// same list is what keeps this from passing by rejecting everything.
func TestParseISOAssetsRejectsAMixedDistroStamp(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "kairos-hadron-core-ubuntu-amd64-generic-v4.3.0-k3sv1.36.4+k0s.0.iso"},
		{Name: "kairos-hadron-core-ubuntu-amd64-generic-v4.3.0-k3sv1.36.4+k3s1.iso"},
	}}

	options := ParseISOAssets(release)
	if len(options) != 1 {
		t.Fatalf("got %d options, want only the well-formed one: %+v", len(options), options)
	}
	if options[0].K8sVersion != "v1.36.4+k3s1" {
		t.Errorf("kept the wrong asset: K8sVersion = %q", options[0].K8sVersion)
	}
}

// TestParseISOAssetsKeepsBothDistrosWhenTheyAgree is the other side of that
// check: a k0s stamp under a k0s prefix has to stay. Without it the check
// above is satisfied by a rule that drops every k0s image, which is the bug
// this PR exists to fix.
func TestParseISOAssetsKeepsBothDistrosWhenTheyAgree(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "kairos-hadron-core-ubuntu-amd64-generic-v4.3.0-k0sv1.34.1+k0s.0.iso"},
		{Name: "kairos-hadron-core-ubuntu-amd64-generic-v4.3.0-k3sv1.36.4+k3s1.iso"},
		{Name: "kairos-hadron-core-ubuntu-amd64-generic-v4.3.0.iso"},
	}}

	options := ParseISOAssets(release)
	if len(options) != 3 {
		t.Fatalf("got %d options, want all three: %+v", len(options), options)
	}
}
