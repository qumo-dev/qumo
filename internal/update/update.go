// Package update implements a self-update mechanism that downloads the latest
// qumo release from GitHub and replaces the running binary after verifying
// its SHA-256 checksum.
package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qumo-dev/qumo/internal/version"
)

const (
	owner = "qumo-dev"
	repo  = "qumo"

	// binaryName is the name of the executable inside the release archive.
	binaryName = "qumo"
)

// apiBaseURL is the root of the GitHub API. It is a variable so tests can
// point it at an httptest server.
var apiBaseURL = "https://api.github.com"

// ghRelease is the subset of the GitHub release JSON we need.
type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Prerelease bool      `json:"prerelease"`
	Draft      bool      `json:"draft"`
	Assets     []ghAsset `json:"assets"`
}

// ghAsset is a single asset attached to a GitHub release.
type ghAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// options holds the parsed CLI flags for the update command.
type options struct {
	checkOnly bool
}

// Run is the entrypoint for `qumo update`. args are the arguments after the
// subcommand name (e.g. ["--check"]).
func Run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	return run(context.Background(), opts)
}

func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	var opts options
	fs.BoolVar(&opts.checkOnly, "check", false, "only check for updates, do not apply")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return opts, nil
}

func run(ctx context.Context, opts options) error {
	cur := version.Version()
	if cur == "dev" {
		fmt.Println("qumo: dev build — skipping update check")
		return nil
	}

	curVer, err := parseSemver(cur)
	if err != nil {
		return fmt.Errorf("cannot parse current version %q: %w", cur, err)
	}

	release, err := detectLatest(ctx, curVer.isPrerelease())
	if err != nil {
		return err
	}

	latestVer, err := parseSemver(release.TagName)
	if err != nil {
		return fmt.Errorf("cannot parse release version %q: %w", release.TagName, err)
	}

	if !latestVer.greaterThan(curVer) {
		fmt.Printf("qumo %s is already up to date\n", cur)
		return nil
	}

	if opts.checkOnly {
		fmt.Printf("qumo %s is available (current: %s)\n", release.TagName, cur)
		return nil
	}

	fmt.Printf("qumo: updating %s → %s ...\n", cur, release.TagName)
	return applyUpdate(ctx, release)
}

// detectLatest fetches the latest suitable release from GitHub. If
// includePrerelease is true it considers pre-releases; otherwise it
// skips them.
func detectLatest(ctx context.Context, includePrerelease bool) (*ghRelease, error) {
	// When we don't need pre-releases, the /latest endpoint is sufficient.
	if !includePrerelease {
		rel, err := fetchRelease(ctx, apiBaseURL+"/repos/"+owner+"/"+repo+"/releases/latest")
		if err != nil {
			return nil, fmt.Errorf("checking for updates: %w", err)
		}
		return rel, nil
	}

	// Otherwise iterate the most recent releases and pick the first
	// non-draft entry (pre-release or stable).
	releases, err := fetchReleases(ctx, apiBaseURL+"/repos/"+owner+"/"+repo+"/releases?per_page=20")
	if err != nil {
		return nil, fmt.Errorf("checking for updates: %w", err)
	}
	for i := range releases {
		if !releases[i].Draft {
			return &releases[i], nil
		}
	}
	return nil, errors.New("no suitable release found")
}

func fetchRelease(ctx context.Context, url string) (*ghRelease, error) {
	body, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	var r ghRelease
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("parsing release JSON: %w", err)
	}
	return &r, nil
}

func fetchReleases(ctx context.Context, url string) ([]ghRelease, error) {
	body, err := httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	var rs []ghRelease
	if err := json.Unmarshal(body, &rs); err != nil {
		return nil, fmt.Errorf("parsing releases JSON: %w", err)
	}
	return rs, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// applyUpdate downloads the correct archive for the current OS/arch,
// verifies its SHA-256 checksum, extracts the binary, and replaces
// the running executable.
func applyUpdate(ctx context.Context, release *ghRelease) error {
	archiveName := assetName(release.TagName)
	archiveAsset, checksumAsset, err := findAssets(release, archiveName)
	if err != nil {
		return err
	}

	// Download archive and checksums in sequence.
	archiveData, err := httpGet(ctx, archiveAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", archiveName, err)
	}

	checksumData, err := httpGet(ctx, checksumAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("downloading checksums.txt: %w", err)
	}

	// Verify checksum.
	if err := verifyChecksum(archiveData, checksumData, archiveName); err != nil {
		return err
	}

	// Extract binary from archive.
	binData, err := extractBinary(archiveData, archiveName)
	if err != nil {
		return fmt.Errorf("extracting binary: %w", err)
	}

	// Replace the running executable.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	if err := replaceBinary(exe, binData); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	fmt.Printf("qumo: updated to %s\n", release.TagName)
	return nil
}

// assetName returns the expected archive file name for the current platform.
func assetName(tag string) string {
	ver := strings.TrimPrefix(tag, "v")
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("qumo_%s_%s_%s.%s", ver, runtime.GOOS, runtime.GOARCH, ext)
}

func findAssets(release *ghRelease, archiveName string) (archive, checksum ghAsset, err error) {
	var foundArchive, foundChecksum bool
	for _, a := range release.Assets {
		switch a.Name {
		case archiveName:
			archive = a
			foundArchive = true
		case "checksums.txt":
			checksum = a
			foundChecksum = true
		}
	}
	if !foundArchive {
		return archive, checksum, fmt.Errorf("release %s has no asset %q", release.TagName, archiveName)
	}
	if !foundChecksum {
		return archive, checksum, fmt.Errorf("release %s has no checksums.txt", release.TagName)
	}
	return archive, checksum, nil
}

// verifyChecksum computes the SHA-256 of data and compares it against the
// expected hash for fileName found in the checksums file.
func verifyChecksum(data, checksums []byte, fileName string) error {
	expected, err := findHash(checksums, fileName)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(data)
	actualHex := hex.EncodeToString(actual[:])
	if actualHex != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", fileName, expected, actualHex)
	}
	return nil
}

// findHash parses a GoReleaser checksums.txt and returns the hex SHA-256
// for the given file name. Each line has the format "<hash>  <filename>".
func findHash(checksums []byte, fileName string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		if parts[1] == fileName {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("no checksum found for %s in checksums.txt", fileName)
}

// extractBinary pulls the qumo binary out of a .tar.gz or .zip archive.
func extractBinary(archiveData []byte, archiveName string) ([]byte, error) {
	if strings.HasSuffix(archiveName, ".zip") {
		return extractFromZip(archiveData)
	}
	return extractFromTarGz(archiveData)
}

func extractFromTarGz(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == binaryName {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}

func extractFromZip(data []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		base := filepath.Base(f.Name)
		if base == binaryName || base == binaryName+".exe" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", binaryName)
}

// replaceBinary atomically replaces the executable at path with newData.
//
// On Unix the rename-over is atomic. On Windows we rename the old binary
// to a .old suffix first (Windows allows renaming an open executable but
// not overwriting it), write the new binary, then remove the .old file.
func replaceBinary(path string, newData []byte) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	// Write new binary to a temp file in the same directory.
	tmp, err := os.CreateTemp(dir, base+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(newData); err != nil {
		_ = tmp.Close()        // not actionable: cleaning up temp file on write error
		_ = os.Remove(tmpPath) // not actionable: cleaning up temp file on write error
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath) // not actionable: cleaning up temp file on close error
		return err
	}

	// Make the new binary executable (no-op on Windows).
	// #nosec G302 -- the executable binary needs to be executable by owner and group/others
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath) // not actionable: cleaning up temp file on chmod error
		return err
	}

	if runtime.GOOS == "windows" {
		// Windows: rename current → .old, rename tmp → current, remove .old.
		oldPath := path + ".old"
		_ = os.Remove(oldPath) // not actionable: best-effort cleanup of any stale .old from previous run
		if err := os.Rename(path, oldPath); err != nil {
			_ = os.Remove(tmpPath) // not actionable: cleaning up temp file on rename error
			return err
		}
		if err := os.Rename(tmpPath, path); err != nil {
			// Attempt rollback.
			_ = os.Rename(oldPath, path) // not actionable: best-effort rollback of old binary
			_ = os.Remove(tmpPath)       // not actionable: cleaning up temp file on rename error
			return err
		}
		_ = os.Remove(oldPath) // not actionable: best-effort cleanup of previous executable
		return nil
	}

	// Unix: atomic rename.
	return os.Rename(tmpPath, path)
}
