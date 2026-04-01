package iso

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Result struct {
	Source     string
	Input      string
	LocalPath  string
	Downloaded bool
}

type ResolveConfig struct {
	LocalPath    string
	SourceURL    string
	DownloadsDir string
	Stdin        io.Reader
	Stdout       io.Writer
}

func Resolve(localPath, sourceURL, downloadsDir string) (*Result, error) {
	return ResolveWithConfig(ResolveConfig{
		LocalPath:    localPath,
		SourceURL:    sourceURL,
		DownloadsDir: downloadsDir,
		Stdin:        os.Stdin,
		Stdout:       os.Stdout,
	})
}

func ResolveWithConfig(cfg ResolveConfig) (*Result, error) {
	if cfg.LocalPath != "" && cfg.SourceURL != "" {
		return nil, fmt.Errorf("provide only one of --iso or --url, not both")
	}

	if cfg.LocalPath == "" && cfg.SourceURL == "" {
		selected, err := interactivePicker(cfg.Stdin, cfg.Stdout)
		if err != nil {
			return nil, err
		}
		cfg.SourceURL = selected.DownloadURL
	}
	if cfg.LocalPath != "" {
		abs, err := filepath.Abs(cfg.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("resolve iso path: %w", err)
		}
		if err := validateLocalISO(abs); err != nil {
			return nil, err
		}
		return &Result{Source: "local", Input: cfg.LocalPath, LocalPath: abs, Downloaded: false}, nil
	}
	if err := os.MkdirAll(cfg.DownloadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create downloads directory: %w", err)
	}
	local, err := downloadISO(cfg.SourceURL, cfg.DownloadsDir, cfg.Stdout)
	if err != nil {
		return nil, err
	}
	return &Result{Source: "url", Input: cfg.SourceURL, LocalPath: local, Downloaded: true}, nil
}

func validateLocalISO(path string) error {
	if strings.ToLower(filepath.Ext(path)) != ".iso" {
		return fmt.Errorf("local file is not an .iso: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat iso path: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("iso path points to a directory: %s", path)
	}
	return nil
}

func downloadISO(rawURL, downloadsDir string, stdout io.Writer) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse iso url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("iso url must be absolute")
	}
	if !strings.HasSuffix(strings.ToLower(u.Path), ".iso") {
		return "", fmt.Errorf("url does not look like an iso: %s", rawURL)
	}

	h := sha256.Sum256([]byte(rawURL))
	base := filepath.Base(u.Path)
	target := filepath.Join(downloadsDir, fmt.Sprintf("%x-%s", h[:6], base))
	if _, err := os.Stat(target); err == nil {
		fmt.Fprintf(stdout, "Using cached ISO: %s\n", base)
		return target, nil
	}

	fmt.Fprintf(stdout, "Downloading %s...\n", base)

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", fmt.Errorf("download iso: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download iso: unexpected status %d", resp.StatusCode)
	}

	f, err := os.Create(target)
	if err != nil {
		return "", fmt.Errorf("create iso file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	totalSize := resp.ContentLength
	if totalSize > 0 {
		pw := &progressWriter{
			total:  totalSize,
			stdout: stdout,
		}
		if _, err := io.Copy(f, io.TeeReader(resp.Body, pw)); err != nil {
			return "", fmt.Errorf("write iso file: %w", err)
		}
		fmt.Fprintln(stdout)
	} else {
		if _, err := io.Copy(f, resp.Body); err != nil {
			return "", fmt.Errorf("write iso file: %w", err)
		}
	}
	return target, nil
}

type progressWriter struct {
	written int64
	total   int64
	stdout  io.Writer
	lastPct int
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n := len(p)
	pw.written += int64(n)
	pct := int(pw.written * 100 / pw.total)
	if pct != pw.lastPct {
		pw.lastPct = pct
		fmt.Fprintf(pw.stdout, "\rDownloading... %d%% (%s / %s)", pct, humanSize(pw.written), humanSize(pw.total))
	}
	return n, nil
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func interactivePicker(stdin io.Reader, stdout io.Writer) (*ISOOption, error) {
	fmt.Fprintln(stdout, "No ISO specified. Fetching latest Kairos releases...")

	release, err := FetchLatestRelease()
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}

	allOptions := ParseISOAssets(release)
	options := FilterByArch(allOptions, "")

	if len(options) == 0 {
		return nil, fmt.Errorf("no compatible ISOs found for your architecture")
	}

	fmt.Fprintf(stdout, "\nKairos %s - Select image type:\n", release.TagName)
	fmt.Fprintln(stdout, "  [1] core     - Base OS only (no Kubernetes)")
	fmt.Fprintln(stdout, "  [2] standard - Includes K3s Kubernetes")
	fmt.Fprint(stdout, "Choice [1-2]: ")

	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	choice := strings.TrimSpace(line)

	if choice == "1" {
		coreOptions := FilterByFlavor(options, "core")
		if len(coreOptions) == 0 {
			return nil, fmt.Errorf("no core ISO found")
		}
		selected := &coreOptions[0]
		fmt.Fprintf(stdout, "\nSelected: %s\n", selected.Name)
		return selected, nil
	}

	if choice != "2" {
		return nil, fmt.Errorf("invalid choice: %s", choice)
	}

	standardOptions := FilterByFlavor(options, "standard")
	if len(standardOptions) == 0 {
		return nil, fmt.Errorf("no standard ISO found")
	}

	k3sVersions := GetK3sVersions(standardOptions)
	if len(k3sVersions) == 0 {
		return nil, fmt.Errorf("no K3s versions found")
	}

	fmt.Fprintln(stdout, "\nSelect K3s version:")
	for i, v := range k3sVersions {
		label := v
		if i == 0 {
			label += " (latest)"
		}
		fmt.Fprintf(stdout, "  [%d] %s\n", i+1, label)
	}
	fmt.Fprintf(stdout, "Choice [1-%d]: ", len(k3sVersions))

	line, err = reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	choice = strings.TrimSpace(line)
	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 1 || idx > len(k3sVersions) {
		return nil, fmt.Errorf("invalid choice: %s", choice)
	}

	selected := FindByK3sVersion(standardOptions, k3sVersions[idx-1])
	if selected == nil {
		return nil, fmt.Errorf("ISO not found for selected version")
	}

	fmt.Fprintf(stdout, "\nSelected: %s\n", selected.Name)
	return selected, nil
}
