package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// Identify the right archive asset for this platform.
	// GoReleaser names archives: forge_{version}_{os}_{arch}.{ext}
	assetName := platformAssetName(stripV(release.TagName))
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
		return fmt.Errorf("no release archive found for %s/%s (expected %q) in %s",
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
	archiveStagingPath := filepath.Join(filepath.Dir(currentBinary), ".forge-update-archive")
	stagingPath := filepath.Join(filepath.Dir(currentBinary), ".forge-update-staging")

	// Download the release archive
	fmt.Printf("Downloading %s...\n", assetName)
	dlCtx, dlCancel := context.WithTimeout(rootCtx, 5*time.Minute)
	defer dlCancel()
	if err := downloadFile(dlCtx, assetURL, archiveStagingPath); err != nil {
		_ = os.Remove(archiveStagingPath)
		return fmt.Errorf("downloading archive: %w", err)
	}

	// Verify checksum of the archive before extracting
	if checksumURL != "" {
		fmt.Println("Verifying checksum...")
		csCtx, csCancel := context.WithTimeout(rootCtx, 30*time.Second)
		csErr := verifyChecksum(csCtx, archiveStagingPath, assetName, checksumURL)
		csCancel()
		if csErr != nil {
			_ = os.Remove(archiveStagingPath)
			return fmt.Errorf("checksum verification: %w", csErr)
		}
		fmt.Println("Checksum OK.")
	}

	// Extract the binary from the archive
	fmt.Println("Extracting binary...")
	if err := extractBinaryFromArchive(archiveStagingPath, platformBinaryInArchive(), stagingPath); err != nil {
		_ = os.Remove(archiveStagingPath)
		_ = os.Remove(stagingPath)
		return fmt.Errorf("extracting binary: %w", err)
	}
	_ = os.Remove(archiveStagingPath)

	// Set executable permissions on non-Windows
	if runtime.GOOS != "windows" {
		if err := os.Chmod(stagingPath, 0o755); err != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("setting executable permissions: %w", err)
		}
	}

	// Stop daemon gracefully if it is running
	_, daemonRunning := daemon.IsRunning()
	if daemonRunning {
		fmt.Println("Stopping daemon...")
		if err := daemon.Stop(); err != nil {
			_ = os.Remove(stagingPath)
			return fmt.Errorf("stopping daemon: %w", err)
		}
		// Wait for the daemon to fully stop before touching the binary.
		stopDeadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(stopDeadline) {
			_, running := daemon.IsRunning()
			if !running {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Replace the binary: backup current, place new
	fmt.Println("Installing update...")
	if err := installUpdate(stagingPath, currentBinary, backupPath); err != nil {
		if errors.Is(err, errWindowsUpdateScheduled) {
			// The swap is handled asynchronously by a helper script on Windows.
			// stagingPath must not be removed — the script will move it into place.
			fmt.Printf("Update to %s scheduled — the binary will be replaced after Forge exits.\n", release.TagName)
			fmt.Println("Run 'forge up' once the update completes to restart the daemon.")
			return nil
		}
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

// platformAssetName returns the release archive name for the current OS and architecture.
// GoReleaser uses: forge_{version}_{os}_{arch}.{ext}
func platformAssetName(version string) string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("forge_%s_%s_%s.%s", version, runtime.GOOS, runtime.GOARCH, ext)
}

// platformBinaryInArchive returns the binary filename inside a release archive.
func platformBinaryInArchive() string {
	if runtime.GOOS == "windows" {
		return "forge.exe"
	}
	return "forge"
}

// extractBinaryFromArchive extracts binaryName from the archive at archivePath into destPath.
func extractBinaryFromArchive(archivePath, binaryName, destPath string) error {
	if runtime.GOOS == "windows" {
		return extractFromZip(archivePath, binaryName, destPath)
	}
	return extractFromTarGz(archivePath, binaryName, destPath)
}

func extractFromZip(archivePath, binaryName, destPath string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		if filepath.Base(f.Name) == binaryName {
			if f.FileInfo().IsDir() || f.FileInfo().Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive entry %q is not a regular file", f.Name)
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			defer out.Close()

			_, err = io.Copy(out, rc)
			return err
		}
	}
	return fmt.Errorf("binary %q not found in zip archive", binaryName)
}

func extractFromTarGz(archivePath, binaryName, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		if filepath.Base(hdr.Name) == binaryName {
			if hdr.Typeflag != tar.TypeReg {
				return fmt.Errorf("archive entry %q is not a regular file", hdr.Name)
			}
			out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			defer out.Close()

			_, err = io.Copy(out, tr)
			return err
		}
	}
	return fmt.Errorf("binary %q not found in tar.gz archive", binaryName)
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

	f, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
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

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksums server returned %s", resp.Status)
	}

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

// errWindowsUpdateScheduled signals that the binary swap has been handed off to
// a helper batch script on Windows (async — completes after the current process exits).
var errWindowsUpdateScheduled = errors.New("windows update scheduled")

// installUpdate backs up the current binary and moves the new one into place.
// On failure it attempts to restore the backup.
// On Windows, if the running exe cannot be renamed directly, it falls back to a
// detached batch script that completes the swap after this process exits.
func installUpdate(newBinary, currentBinary, backupPath string) error {
	// Remove any stale backup left by a previous failed update so that the
	// rename below does not fail with "file exists".
	_ = os.Remove(backupPath)

	if err := os.Rename(currentBinary, backupPath); err != nil {
		if runtime.GOOS == "windows" {
			// The running .exe may be locked on Windows. Delegate the swap to a
			// detached helper script that runs after this process exits.
			return scheduleWindowsUpdate(newBinary, currentBinary, backupPath)
		}
		return fmt.Errorf("backing up current binary to %s: %w", backupPath, err)
	}
	if err := os.Rename(newBinary, currentBinary); err != nil {
		_ = os.Rename(backupPath, currentBinary)
		return fmt.Errorf("placing new binary: %w", err)
	}
	return nil
}

// scheduleWindowsUpdate writes a batch script that waits for the current process
// to exit and then performs the binary swap. The script is launched detached and
// this function returns errWindowsUpdateScheduled so the caller can exit cleanly.
func scheduleWindowsUpdate(newBinary, currentBinary, backupPath string) error {
	pid := os.Getpid()
	scriptPath := filepath.Join(filepath.Dir(currentBinary), ".forge-update.bat")

	// The script retries the move until the forge process has released the file,
	// then cleans itself up.
	script := fmt.Sprintf(`@echo off
setlocal
:waitloop
tasklist /fi "PID eq %d" 2>nul | findstr /i /c:"%d" >nul
if not errorlevel 1 (
    ping -n 2 127.0.0.1 >nul
    goto waitloop
)
move /Y "%s" "%s"
if errorlevel 1 exit /b 1
move /Y "%s" "%s"
if errorlevel 1 (
    move /Y "%s" "%s"
    exit /b 1
)
del "%%~f0"
`, pid, pid, currentBinary, backupPath, newBinary, currentBinary, backupPath, currentBinary)

	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		return fmt.Errorf("creating update script: %w", err)
	}

	cmd := exec.Command("cmd.exe", "/C", scriptPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(scriptPath)
		return fmt.Errorf("launching update script: %w", err)
	}
	_ = cmd.Process.Release()
	return errWindowsUpdateScheduled
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
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	c := updateCache{TagName: tagName, CheckedAt: time.Now()}
	data, _ := json.Marshal(c)
	_ = os.WriteFile(p, data, 0o644)
}

// printUpdateHint reads the local update cache and prints a hint if a newer release is cached.
// If the cache is missing or stale (> 1h), it refreshes synchronously with a short timeout so
// that the cache is populated before the process exits.
func printUpdateHint() {
	cache := readUpdateCache()

	// Refresh cache synchronously (with a short timeout) if missing or stale, so the
	// data is written to disk before the process exits rather than being lost when a
	// background goroutine is killed on exit.
	if cache == nil || time.Since(cache.CheckedAt) > updateCacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		release, err := getLatestRelease(ctx)
		if err == nil {
			writeUpdateCache(release.TagName)
			cache = &updateCache{TagName: release.TagName, CheckedAt: time.Now()}
		}
		// Network errors are silently swallowed — the hint is best-effort.
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
