package iso

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	Source     string
	Input      string
	LocalPath  string
	Downloaded bool
}

func Resolve(localPath, sourceURL, downloadsDir string) (*Result, error) {
	if (localPath == "" && sourceURL == "") || (localPath != "" && sourceURL != "") {
		return nil, fmt.Errorf("provide exactly one of --iso or --url")
	}
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
	if err := os.MkdirAll(downloadsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create downloads directory: %w", err)
	}
	local, err := downloadISO(sourceURL, downloadsDir)
	if err != nil {
		return nil, err
	}
	return &Result{Source: "url", Input: sourceURL, LocalPath: local, Downloaded: true}, nil
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

func downloadISO(rawURL, downloadsDir string) (string, error) {
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
		return target, nil
	}

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
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("write iso file: %w", err)
	}
	return target, nil
}
