package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-github/v83/github"

	"github.com/infracost/lsp/version"
)

const (
	repoOwner  = "infracost"
	repoName   = "lsp"
	binaryName = "infracost-ls"

	UpdateCheckTimeout = 10 * time.Second

	// maxAssetSize is the maximum size of a release asset we'll download (100MB).
	maxAssetSize = 100 << 20

	updateTmpPrefix = ".infracost-ls-update-"
	updateOldPrefix = ".infracost-ls-old-"

	// leftovers younger than this may belong to a swap still running in
	// another process
	staleAfter = 10 * time.Minute
)

// test seam for the rollback path
var renameFile = os.Rename

// CheckResult contains the result of a version check.
type CheckResult struct {
	UpdateAvailable bool   `json:"updateAvailable"`
	LatestVersion   string `json:"latestVersion"`
	CurrentVersion  string `json:"currentVersion"`
}

// Check queries GitHub for the latest release and reports whether an update is available.
func Check(ctx context.Context) (*CheckResult, error) {
	return check(ctx, nil)
}

// Update downloads and installs the latest release, replacing the current binary.
func Update(ctx context.Context) (*CheckResult, error) {
	var release *github.RepositoryRelease
	result, err := check(ctx, &release)
	if err != nil {
		return nil, err
	}
	if !result.UpdateAvailable {
		return result, nil
	}

	if err := downloadAndReplace(ctx, release); err != nil {
		return nil, err
	}

	return result, nil
}

func check(ctx context.Context, releaseOut **github.RepositoryRelease) (*CheckResult, error) {
	current, _ := semver.NewVersion(version.Version)

	client := newGitHubClient()
	release, _, err := client.Repositories.GetLatestRelease(ctx, repoOwner, repoName)
	if err != nil {
		return nil, fmt.Errorf("fetching latest release: %w", err)
	}

	if releaseOut != nil {
		*releaseOut = release
	}

	tag := release.GetTagName()
	latest, err := semver.NewVersion(tag)
	if err != nil {
		return nil, fmt.Errorf("parsing release version %q: %w", tag, err)
	}

	result := &CheckResult{
		CurrentVersion: version.Version,
		LatestVersion:  latest.String(),
	}

	if current == nil || latest.GreaterThan(current) {
		result.UpdateAvailable = true
	}

	return result, nil
}

func downloadAndReplace(ctx context.Context, release *github.RepositoryRelease) error {
	assetName := expectedAssetName()
	var assetID int64
	for _, a := range release.Assets {
		if a.GetName() == assetName {
			assetID = a.GetID()
			break
		}
	}
	if assetID == 0 {
		return fmt.Errorf("no release asset for %s/%s (expected %s)", runtime.GOOS, runtime.GOARCH, assetName)
	}

	client := newGitHubClient()
	rc, _, err := client.Repositories.DownloadReleaseAsset(ctx, repoOwner, repoName, assetID, &http.Client{Timeout: 60 * time.Second})
	if err != nil {
		return fmt.Errorf("downloading asset: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, maxAssetSize))
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("reading asset: %w", err)
	}

	bin := binaryName
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	binData, err := extractBinary(assetName, data, bin)
	if err != nil {
		return fmt.Errorf("extracting binary: %w", err)
	}

	if err := replaceBinary(binData); err != nil {
		return fmt.Errorf("replacing binary: %w", err)
	}

	return nil
}

func expectedAssetName() string {
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s.%s", repoName, runtime.GOOS, runtime.GOARCH, ext)
}

func newGitHubClient() *github.Client {
	return github.NewClient(nil)
}

func extractBinary(assetName string, data []byte, name string) ([]byte, error) {
	if strings.HasSuffix(assetName, ".zip") {
		return extractFromZip(data, name)
	}
	return extractFromTarGz(data, name)
}

func extractFromTarGz(data []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", name)
}

func extractFromZip(data []byte, name string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if filepath.Base(f.Name) == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", name)
}

// CleanupStale removes update leftovers beside the running executable.
// Best-effort: a file another process still holds is skipped.
func CleanupStale() {
	execPath, err := resolveExecutable()
	if err != nil {
		slog.Debug("update cleanup skipped", "error", err)
		return
	}
	if err := cleanupStale(filepath.Dir(execPath)); err != nil {
		slog.Debug("update cleanup incomplete", "error", err)
	}
}

var replaceBinary = func(newBinary []byte) error {
	execPath, err := resolveExecutable()
	if err != nil {
		return err
	}

	info, err := os.Stat(execPath)
	if err != nil {
		return err
	}

	return swapBinary(execPath, newBinary, info.Mode().Perm(), runtime.GOOS == "windows")
}

func resolveExecutable() (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(execPath)
}

// swapBinary installs newBinary at execPath. When renameAside is set the file
// already there is renamed out of the way first: Windows refuses to overwrite a
// running executable but allows renaming it. Elsewhere a single rename is atomic
// and never leaves execPath missing, so that is used instead. The caller passes
// the choice rather than swapBinary reading runtime.GOOS so both paths are
// testable on a Linux-only CI.
func swapBinary(execPath string, newBinary []byte, mode fs.FileMode, renameAside bool) error {
	dir := filepath.Dir(execPath)

	tmpPath, err := writeTemp(dir, newBinary, mode)
	if err != nil {
		return err
	}

	if !renameAside {
		if err := renameFile(tmpPath, execPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("installing new binary: %w", err)
		}
		return nil
	}

	backupPath, err := reserveName(dir, updateOldPrefix)
	if err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := renameFile(execPath, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		_ = os.Remove(backupPath)
		return fmt.Errorf("moving current binary aside: %w", err)
	}

	if err := renameFile(tmpPath, execPath); err != nil {
		if rbErr := renameFile(backupPath, execPath); rbErr != nil {
			kept := preserveBackup(execPath, backupPath)
			_ = os.Remove(tmpPath)
			return fmt.Errorf("installing new binary: %w (rollback failed, previous binary left at %s: %v)", err, kept, rbErr)
		}
		_ = os.Remove(tmpPath)
		return fmt.Errorf("installing new binary: %w", err)
	}

	// fails on Windows while this process holds the image; CleanupStale gets it
	_ = os.Remove(backupPath)

	return nil
}

// preserveBackup moves the displaced binary to a name the startup sweep ignores,
// so the path reported to the user is still there when they act on it.
func preserveBackup(execPath, backupPath string) string {
	kept := execPath + ".bak"
	if err := renameFile(backupPath, kept); err != nil {
		return backupPath
	}
	return kept
}

func writeTemp(dir string, data []byte, mode fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(dir, updateTmpPrefix+"*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	// the backup goes as soon as the rename lands, so the bytes must be on disk
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	return tmpPath, nil
}

// reserveName reserves a unique name. os.Rename replaces the placeholder.
func reserveName(dir, prefix string) (string, error) {
	f, err := os.CreateTemp(dir, prefix+"*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

func cleanupStale(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}

	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, updateOldPrefix) && !strings.HasPrefix(name, updateTmpPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(time.Now().Add(-staleAfter)) {
			continue
		}
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil {
			errs = append(errs, err)
			continue
		}
		slog.Debug("removed stale update file", "path", path)
	}

	return errors.Join(errs...)
}
