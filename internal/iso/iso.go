package iso

import (
	"bufio"
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

type DownloadConfig struct {
	DownloadsDir string
	Stdin        io.Reader
	Stdout       io.Writer
}

// Download interactively selects and downloads an ISO, returning the path
func Download(cfg DownloadConfig) (string, error) {
	selected, err := interactivePicker(cfg.Stdin, cfg.Stdout)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(cfg.DownloadsDir, 0o755); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}

	localPath, err := downloadISO(selected.DownloadURL, cfg.DownloadsDir, cfg.Stdout)
	if err != nil {
		return "", err
	}

	_, _ = fmt.Fprintf(cfg.Stdout, "\nISO saved to: %s\n", localPath)
	return localPath, nil
}

// ListDownloaded returns all ISO files in the downloads directory
func ListDownloaded(downloadsDir string) ([]string, error) {
	entries, err := os.ReadDir(downloadsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var isos []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToLower(e.Name()), ".iso") {
			isos = append(isos, filepath.Join(downloadsDir, e.Name()))
		}
	}
	return isos, nil
}

type SelectConfig struct {
	DownloadsDir string
	Stdin        io.Reader
	Stdout       io.Writer
}

// SelectDownloaded prompts user to select from downloaded ISOs if multiple exist
// Returns error if no ISOs available
func SelectDownloaded(cfg SelectConfig) (string, error) {
	isos, err := ListDownloaded(cfg.DownloadsDir)
	if err != nil {
		return "", err
	}

	if len(isos) == 0 {
		return "", fmt.Errorf("no ISOs found. Run 'kairos-lab download' first")
	}

	if len(isos) == 1 {
		return isos[0], nil
	}

	_, _ = fmt.Fprintln(cfg.Stdout, "Multiple ISOs found. Select one:")
	for i, iso := range isos {
		_, _ = fmt.Fprintf(cfg.Stdout, "  [%d] %s\n", i+1, filepath.Base(iso))
	}
	_, _ = fmt.Fprintf(cfg.Stdout, "Choice [1-%d] (or Ctrl-c to cancel and download a different one): ", len(isos))

	reader := bufio.NewReader(cfg.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("cancelled")
	}

	choice := strings.TrimSpace(line)
	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 1 || idx > len(isos) {
		return "", fmt.Errorf("invalid choice: %s", choice)
	}

	return isos[idx-1], nil
}

// SelectOrDownloadISO lets the user pick from existing downloaded ISOs or download a new one.
func SelectOrDownloadISO(downloadsDir string, stdin io.Reader, stdout io.Writer) (string, error) {
	isos, err := ListDownloaded(downloadsDir)
	if err != nil {
		return "", err
	}

	reader := bufio.NewReader(stdin)

	if len(isos) > 0 {
		_, _ = fmt.Fprintln(stdout, "Available ISOs:")
		for i, iso := range isos {
			_, _ = fmt.Fprintf(stdout, "  [%d] %s\n", i+1, filepath.Base(iso))
		}
		_, _ = fmt.Fprintln(stdout, "  [n] Download new ISO")
		_, _ = fmt.Fprintf(stdout, "Choice [1-%d or n]: ", len(isos))

		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("cancelled")
		}
		choice := strings.TrimSpace(strings.ToLower(line))

		if choice != "n" && choice != "new" {
			idx, err := strconv.Atoi(choice)
			if err != nil || idx < 1 || idx > len(isos) {
				return "", fmt.Errorf("invalid choice: %s", choice)
			}
			return isos[idx-1], nil
		}
	}

	// Download new ISO
	if err := os.MkdirAll(downloadsDir, 0o755); err != nil {
		return "", fmt.Errorf("create downloads directory: %w", err)
	}
	selected, err := interactivePicker(stdin, stdout)
	if err != nil {
		return "", err
	}
	localPath, err := downloadISO(selected.DownloadURL, downloadsDir, stdout)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintf(stdout, "\nISO saved to: %s\n", localPath)
	return localPath, nil
}

// ResolveForStart resolves an ISO for starting a VM
// If localPath is provided, uses that. Otherwise selects from downloaded ISOs.
func ResolveForStart(localPath, downloadsDir string, stdin io.Reader, stdout io.Writer) (*Result, error) {
	if localPath != "" {
		abs, err := filepath.Abs(localPath)
		if err != nil {
			return nil, fmt.Errorf("resolve iso path: %w", err)
		}
		if err := validateLocalISO(abs); err != nil {
			return nil, err
		}
		return &Result{Source: "local", Input: localPath, LocalPath: abs, Downloaded: false}, nil
	}

	selected, err := SelectDownloaded(SelectConfig{
		DownloadsDir: downloadsDir,
		Stdin:        stdin,
		Stdout:       stdout,
	})
	if err != nil {
		return nil, err
	}

	return &Result{Source: "downloaded", Input: selected, LocalPath: selected, Downloaded: false}, nil
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

	base := filepath.Base(u.Path)
	target := filepath.Join(downloadsDir, base)
	if _, err := os.Stat(target); err == nil {
		_, _ = fmt.Fprintf(stdout, "Using cached ISO: %s\n", base)
		return target, nil
	}

	_, _ = fmt.Fprintf(stdout, "Downloading %s...\n", base)

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
		_, _ = fmt.Fprintln(stdout)
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
		_, _ = fmt.Fprintf(pw.stdout, "\rDownloading... %d%% (%s / %s)", pct, humanSize(pw.written), humanSize(pw.total))
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
	_, _ = fmt.Fprintln(stdout, "No ISO specified. Fetching latest Kairos releases...")

	release, err := FetchLatestRelease()
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}

	allOptions := ParseISOAssets(release)
	options := FilterByArch(allOptions, "")

	if len(options) == 0 {
		return nil, fmt.Errorf("no compatible ISOs found for your architecture")
	}

	_, _ = fmt.Fprintf(stdout, "\nKairos %s - Select image type:\n", release.TagName)
	_, _ = fmt.Fprintln(stdout, "  [1] core     - Base OS only (no Kubernetes)")
	_, _ = fmt.Fprintln(stdout, "  [2] standard - Includes K3s Kubernetes")
	_, _ = fmt.Fprint(stdout, "Choice [1-2]: ")

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
		_, _ = fmt.Fprintf(stdout, "\nSelected: %s\n", selected.Name)
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

	_, _ = fmt.Fprintln(stdout, "\nSelect K3s version:")
	for i, v := range k3sVersions {
		label := v
		if i == 0 {
			label += " (latest)"
		}
		_, _ = fmt.Fprintf(stdout, "  [%d] %s\n", i+1, label)
	}
	_, _ = fmt.Fprintf(stdout, "Choice [1-%d]: ", len(k3sVersions))

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

	_, _ = fmt.Fprintf(stdout, "\nSelected: %s\n", selected.Name)
	return selected, nil
}
