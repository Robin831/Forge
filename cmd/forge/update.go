package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/daemon"
	"github.com/Robin831/Forge/internal/forge"
	"github.com/spf13/cobra"
)

const githubLatestReleaseURL = "https://api.github.com/repos/Robin831/Forge/releases/latest"

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func init() {
	updateCmd.Flags().Bool("check", false, "Only check for updates, do not install")
	rootCmd.AddCommand(updateCmd)
}

var updateCmd = &cobra.Command{
	Use:     "update",
	Short:   "Download and install the latest Forge release",
	Long:    "Downloads the latest release binary from GitHub and replaces the current binary.\nThe daemon is gracefully stopped before the update and restarted afterwards.\nOn failure, the previous binary is restored from forge.bak.",
	GroupID: "daemon",
	RunE:    runUpdate,
}

func runUpdate(cmd *cobra.Command, args []string) error {
	checkOnly, _ := cmd.Flags().GetBool("check")

	fmt.Printf("Current version: %s\n", forge.Version)
	fmt.Println("Checking for latest release...")

	ctx, cancel := context.WithTimeout(rootCtx, 15*time.Second)
	defer cancel()

	release, err := getLatestRelease(ctx)
	if err != nil {
		return fmt.Errorf("checking latest release: %w", err)
	}

	current := stripV(forge.Version)
	latest := stripV(release.TagName)
	isNewer := current == "dev" || compareVersions(current, latest) < 0

	if !isNewer {
		fmt.Printf("Already up to date (%s)\n", release.TagName)
		return nil
	}

	fmt.Printf("New version available: %s → %s\n", forge.Version, release.TagName)

	if checkOnly {
		return nil
	}

	// Identify the right binary asset for this platform
	assetName := platformAssetName()
	var assetURL, checksumURL string
	for _, asset := range release.Assets {
		switch asset.Name {
		case assetName:
			assetURL = asset.BrowserDownloadURL
		case "checksums.txt", "SHA256SUMS":
			checksumURL = asset.BrowserDownloadURL
		}
	}

	if assetURL == "" {
		return fmt.Errorf("no release binary found for %s/%s (expected %q) in %s",
			runtime.GOOS, runtime.GOARCH, assetName, release.TagName)
	}

	// Resolve current binary path (follow symlinks)
	currentBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current binary: %w", err)
	}
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return fmt.Errorf("resolving binary path: %w", err)
	}

	backupPath := currentBinary + ".bak"
	// Stage in the same directory to avoid cross-device rename failures
	stagingPath := filepath.Join(filepath.Dir(currentBinary), ".forge-update-staging")

	// Download to staging path
	fmt.Printf("Downloading %s...\n", assetName)
	dlCtx, dlCancel := context.WithTimeout(rootCtx, 5*time.Minute)
	defer dlCancel()
	if err := downloadFile(dlCtx, assetURL, stagingPath); err != nil {
		_ = os.Remove(stagingPath)
		return fmt.Errorf("downloading binary: %w", err)
	}

	// Set executable permissions on non-Windows
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stagingPath, 0755); err != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("setting executable permissions: %w", err)
		}
	}

	// Verify checksum if a checksums file is available
	if checksumURL != "" {
		fmt.Println("Verifying checksum...")
		csCtx, csCancel := context.WithTimeout(rootCtx, 30*time.Second)
		csErr := verifyChecksum(csCtx, stagingPath, assetName, checksumURL)
		csCancel()
		if csErr != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("checksum verification: %w", csErr)
		}
		fmt.Println("Checksum OK.")
	}

	// Stop daemon gracefully if it is running
	_, daemonRunning := daemon.IsRunning()
	if daemonRunning {
		fmt.Println("Stopping daemon...")
		if err := daemon.Stop(); err != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("stopping daemon: %w", err)
		}
		// Brief pause for graceful shutdown
		time.Sleep(2 * time.Second)
	}

	// Replace the binary: backup current, place new
	fmt.Println("Installing update...")
	if err := installUpdate(stagingPath, currentBinary, backupPath); err != nil {
		_ = os.Remove(stagingPath)
		if daemonRunning {
			_ = startDaemon(currentBinary)
		}
		return fmt.Errorf("installing update: %w", err)
	}

	_ = os.Remove(stagingPath)
	fmt.Printf("Successfully updated to %s.\n", release.TagName)

	// Restart daemon if it was running before the update.
	// Keep the backup alive until the daemon confirms a clean start so the user
	// can restore it manually if the new binary misbehaves.
	if daemonRunning {
		fmt.Println("Restarting daemon...")
		if err := startDaemon(currentBinary); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not restart daemon: %v\n", err)
			fmt.Fprintf(os.Stderr, "Previous binary preserved at %s — restore it manually if needed.\n", backupPath)
			fmt.Println("Run 'forge up' to start the daemon manually.")
		} else {
			fmt.Println("Daemon restarted.")
			// Remove backup only after the daemon has started with the new binary.
			_ = os.Remove(backupPath)
		}
	} else {
		// Daemon was not running; safe to remove backup immediately.
		_ = os.Remove(backupPath)
	}

	return nil
}

// getLatestRelease fetches the latest GitHub release metadata.
func getLatestRelease(ctx context.Context) (*githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubLatestReleaseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "forge/"+forge.Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API responded with %s", resp.Status)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}
	if release.TagName == "" {
		return nil, fmt.Errorf("no tag_name in GitHub response")
	}
	return &release, nil
}

// stripV removes a leading "v" from a version string.
func stripV(v string) string {
	return strings.TrimPrefix(v, "v")
}

// compareVersions compares two semver strings (without "v" prefix).
// Returns -1 if a < b, 0 if equal, 1 if a > b.
func compareVersions(a, b string) int {
	ap := parseSemver(a)
	bp := parseSemver(b)
	for i := range ap {
		if ap[i] < bp[i] {
			return -1
		}
		if ap[i] > bp[i] {
			return 1
		}
	}
	return 0
}

func parseSemver(v string) [3]int {
	var major, minor, patch int
	fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
	return [3]int{major, minor, patch}
}

// platformAssetName returns the expected release binary name for the current OS and architecture.
func platformAssetName() string {
	name := fmt.Sprintf("forge-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "forge/"+forge.Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

// verifyChecksum downloads a checksums file and verifies the SHA256 of the binary.
func verifyChecksum(ctx context.Context, binaryPath, binaryName, checksumURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "forge/"+forge.Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading checksums: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading checksums: %w", err)
	}

	var expected string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Checksums format: "<hash>  <filename>" or "<hash> <filename>"
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == binaryName {
			expected = strings.ToLower(parts[0])
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("no checksum entry for %q in checksums file", binaryName)
	}

	f, err := os.Open(binaryPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))

	if actual != expected {
		return fmt.Errorf("SHA256 mismatch: got %s, want %s", actual, expected)
	}
	return nil
}

// installUpdate backs up the current binary and moves the new one into place.
// On failure it attempts to restore the backup.
func installUpdate(newBinary, currentBinary, backupPath string) error {
	if err := os.Rename(currentBinary, backupPath); err != nil {
		return fmt.Errorf("backing up current binary to %s: %w", backupPath, err)
	}
	if err := os.Rename(newBinary, currentBinary); err != nil {
		_ = os.Rename(backupPath, currentBinary)
		return fmt.Errorf("placing new binary: %w", err)
	}
	return nil
}

// startDaemon launches the daemon in the background.
func startDaemon(exe string) error {
	args := []string{"up"}
	if configFile != "" {
		args = append(args, "--config", configFile)
	}
	bgCmd := exec.Command(exe, args...)
	bgCmd.Stdout = nil
	bgCmd.Stderr = nil
	bgCmd.Stdin = nil
	detachProcess(bgCmd)
	if err := bgCmd.Start(); err != nil {
		return err
	}
	_ = bgCmd.Process.Release()
	return nil
}

// updateCache is the on-disk cache for the latest known release tag.
type updateCache struct {
	TagName   string    `json:"tag_name"`
	CheckedAt time.Time `json:"checked_at"`
}

const updateCacheTTL = 1 * time.Hour

func forgeUpdateCachePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".forge", "update-cache.json")
}

func readUpdateCache() *updateCache {
	data, err := os.ReadFile(forgeUpdateCachePath())
	if err != nil {
		return nil
	}
	var c updateCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	return &c
}

func writeUpdateCache(tagName string) {
	p := forgeUpdateCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0755)
	c := updateCache{TagName: tagName, CheckedAt: time.Now()}
	data, _ := json.Marshal(c)
	_ = os.WriteFile(p, data, 0644)
}

// printUpdateHint reads the local update cache and prints a hint if a newer release is cached.
// If the cache is missing or stale (> 1h), a background goroutine refreshes it for the next run.
// This function never makes a synchronous network call, so it does not add latency to forge status.
func printUpdateHint() {
	cache := readUpdateCache()

	// Refresh cache in background if missing or stale.
	if cache == nil || time.Since(cache.CheckedAt) > updateCacheTTL {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			release, err := getLatestRelease(ctx)
			if err != nil {
				return
			}
			writeUpdateCache(release.TagName)
		}()
	}

	if cache == nil {
		return
	}

	current := stripV(forge.Version)
	latest := stripV(cache.TagName)
	if current == "dev" || compareVersions(current, latest) < 0 {
		fmt.Printf("\n%s available — run forge update\n", cache.TagName)
	}
}
