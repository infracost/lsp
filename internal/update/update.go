package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
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
)

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

var replaceBinary = func(newBinary []byte) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(execPath)
	if err != nil {
		return err
	}

	dir := filepath.Dir(execPath)
	tmp, err := os.CreateTemp(dir, ".infracost-ls-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(newBinary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	if err := os.Rename(tmpPath, execPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}

	return nil
}
