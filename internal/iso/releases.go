package iso

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
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
			Size:        asset.Size,
			Flavor:      matches[1],
			Arch:        matches[2],
			K3sVersion:  matches[4],
		}
		options = append(options, opt)
	}
	return options
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
		return versions[i] > versions[j]
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
