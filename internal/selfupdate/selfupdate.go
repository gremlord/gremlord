// Package selfupdate implements `gremlord update` — fetching the latest
// GitHub release and replacing the running binary with it.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const repo = "gremlord/gremlord"

// Release is the subset of the GitHub releases API response we need.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Latest fetches the latest published release from GitHub.
func Latest(ctx context.Context) (Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo), nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("checking latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("checking latest release: unexpected status %s", resp.Status)
	}

	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("parsing release response: %w", err)
	}
	if rel.TagName == "" {
		return Release{}, fmt.Errorf("no published release found for %s", repo)
	}
	return rel, nil
}

// AssetName returns the release asset filename for the running OS/arch,
// matching the build matrix in .github/workflows/release.yml.
func AssetName() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64":
	default:
		return "", fmt.Errorf("no release build published for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	return fmt.Sprintf("gremlord-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext), nil
}

// Download fetches the named release's binary for the current OS/arch to
// destPath and marks it executable.
func Download(ctx context.Context, tag, destPath string) error {
	asset, err := AssetName()
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, asset)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", asset, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: unexpected status %s (does release %s exist for this platform?)", asset, resp.Status, tag)
	}

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return fmt.Errorf("writing %s: %w", destPath, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(destPath, 0o755)
}

// Apply replaces the binary at exePath with the one at newPath. The current
// binary is moved aside first rather than overwritten in place — both Unix
// and Windows leave an already-running executable usable after it's been
// renamed or unlinked, so this works even when exePath is gremlord's own
// currently-executing binary.
func Apply(exePath, newPath string) error {
	dir := filepath.Dir(exePath)
	oldPath := filepath.Join(dir, ".gremlord.old")
	os.Remove(oldPath) // best effort, leftover from a previous update

	if err := os.Rename(exePath, oldPath); err != nil {
		return fmt.Errorf("moving current binary aside: %w", err)
	}
	if err := os.Rename(newPath, exePath); err != nil {
		os.Rename(oldPath, exePath) // best-effort rollback
		return fmt.Errorf("installing new binary: %w", err)
	}
	os.Remove(oldPath) // best effort; may still be locked on Windows
	return nil
}
