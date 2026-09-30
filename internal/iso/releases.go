package iso

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	releasesAPIURL = "https://api.github.com/repos/kairos-io/kairos/releases/latest"
)

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type ISOOption struct {
	Name        string
	DownloadURL string
	// ChecksumURL is the "<Name>.sha256" asset of the same release, empty when
	// the release does not publish one for this ISO.
	ChecksumURL string
	Size        int64
	Flavor      string // "core" or "standard"
	Arch        string // "amd64" or "arm64"
	K3sVersion  string // empty for core, e.g. "v1.35.2+k3s1" for standard
}

func FetchLatestRelease() (*Release, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(releasesAPIURL)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch releases: unexpected status %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	return &release, nil
}

var isoNamePattern = regexp.MustCompile(`^kairos-hadron-[^-]+-(\w+)-(amd64|arm64)-generic-v[\d.]+(-(k3sv[\d.]+\+k3s\d+))?\.iso$`)

func ParseISOAssets(release *Release) []ISOOption {
	// Every ISO the release publishes ships a "<name>.sha256" next to it. Pair
	// them up here, while both are still in the same asset list, so the
	// download has a digest to check against without guessing a URL.
	checksums := make(map[string]string, len(release.Assets))
	for _, asset := range release.Assets {
		if name, ok := strings.CutSuffix(asset.Name, ".sha256"); ok {
			checksums[name] = asset.BrowserDownloadURL
		}
	}

	var options []ISOOption
	for _, asset := range release.Assets {
		if !strings.HasSuffix(asset.Name, ".iso") {
			continue
		}
		matches := isoNamePattern.FindStringSubmatch(asset.Name)
		if matches == nil {
			continue
		}
		opt := ISOOption{
			Name:        asset.Name,
			DownloadURL: asset.BrowserDownloadURL,
			ChecksumURL: checksums[asset.Name],
			Size:        asset.Size,
			Flavor:      matches[1],
			Arch:        matches[2],
			K3sVersion:  matches[4],
		}
		options = append(options, opt)
	}
	return options
}

// FetchChecksum returns the SHA-256 the release publishes for this ISO.
//
// A release that publishes no checksum for an ISO is an error rather than a
// skipped check: the digest is what makes the downloaded image trustworthy,
// and silently falling back to "whatever arrived" is the behaviour this
// replaces (kairos-io/kairos#5009).
func FetchChecksum(opt *ISOOption) (string, error) {
	if opt.ChecksumURL == "" {
		return "", fmt.Errorf("the release publishes no .sha256 for %s, so the download cannot be verified", opt.Name)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(opt.ChecksumURL)
	if err != nil {
		return "", fmt.Errorf("fetch checksum for %s: %w", opt.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch checksum for %s: unexpected status %d", opt.Name, resp.StatusCode)
	}

	// A .sha256 is one short line. Cap the read so a wrong URL cannot stream
	// an ISO into memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read checksum for %s: %w", opt.Name, err)
	}

	digest, err := parseSHA256Sum(string(body), opt.Name)
	if err != nil {
		return "", fmt.Errorf("parse checksum for %s: %w", opt.Name, err)
	}
	return digest, nil
}

func FilterByArch(options []ISOOption, arch string) []ISOOption {
	if arch == "" {
		arch = runtime.GOARCH
	}
	var filtered []ISOOption
	for _, opt := range options {
		if opt.Arch == arch {
			filtered = append(filtered, opt)
		}
	}
	return filtered
}

func FilterByFlavor(options []ISOOption, flavor string) []ISOOption {
	var filtered []ISOOption
	for _, opt := range options {
		if opt.Flavor == flavor {
			filtered = append(filtered, opt)
		}
	}
	return filtered
}

func GetK3sVersions(options []ISOOption) []string {
	seen := make(map[string]bool)
	var versions []string
	for _, opt := range options {
		if opt.K3sVersion != "" && !seen[opt.K3sVersion] {
			seen[opt.K3sVersion] = true
			versions = append(versions, opt.K3sVersion)
		}
	}
	sort.Slice(versions, func(i, j int) bool {
		return compareK8sVersions(versions[i], versions[j]) > 0
	})
	return versions
}

func FindByK3sVersion(options []ISOOption, k3sVersion string) *ISOOption {
	for _, opt := range options {
		if opt.K3sVersion == k3sVersion {
			return &opt
		}
	}
	return nil
}

func FindCore(options []ISOOption) *ISOOption {
	for _, opt := range options {
		if opt.Flavor == "core" {
			return &opt
		}
	}
	return nil
}

var (
	// k8sVersionPattern splits a version the way a release names it,
	// "k3sv1.35.8+k3s1" or "k0sv1.36.4+k0s.0", into the three numbers of the
	// Kubernetes version and the build suffix of the distribution.
	k8sVersionPattern = regexp.MustCompile(`^[a-z0-9]*v(\d+)\.(\d+)\.(\d+)\+(.+)$`)
	// buildNumberPattern takes the build number off that suffix: the "1" of
	// "k3s1", the "0" of "k0s.0". The distribution name carries a digit of its
	// own, so the build number is the run of digits the suffix ends with, not
	// every digit in it.
	buildNumberPattern = regexp.MustCompile(`\d+$`)
)

// parseK8sVersion returns the numbers that order a version, and whether the
// string had the shape a release publishes.
func parseK8sVersion(version string) ([4]int, bool) {
	matches := k8sVersionPattern.FindStringSubmatch(version)
	if matches == nil {
		return [4]int{}, false
	}
	build := buildNumberPattern.FindString(matches[4])
	if build == "" {
		return [4]int{}, false
	}

	var parts [4]int
	for i, field := range [4]string{matches[1], matches[2], matches[3], build} {
		n, err := strconv.Atoi(field)
		if err != nil {
			return [4]int{}, false
		}
		parts[i] = n
	}
	return parts, true
}

// compareK8sVersions orders two versions by number instead of by character, so
// v1.10.0 is newer than v1.9.0 and +k3s10 is newer than +k3s2. It returns a
// negative number when a is older than b, a positive one when it is newer, and
// zero when they are the same version.
//
// A version this cannot read counts as older than any it can, so a name the
// asset pattern lets through can never take the "(latest)" marker from a real
// one. Names that compare equal by number, and pairs neither of which can be
// read, fall back to character order, which keeps the list the picker prints
// stable across runs of the same release.
func compareK8sVersions(a, b string) int {
	partsA, okA := parseK8sVersion(a)
	partsB, okB := parseK8sVersion(b)
	switch {
	case okA && !okB:
		return 1
	case !okA && okB:
		return -1
	case !okA && !okB:
		return strings.Compare(a, b)
	}
	for i := range partsA {
		switch {
		case partsA[i] < partsB[i]:
			return -1
		case partsA[i] > partsB[i]:
			return 1
		}
	}
	return strings.Compare(a, b)
}
