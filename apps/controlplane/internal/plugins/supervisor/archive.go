package supervisor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/grendalaget/varde/apps/controlplane/internal/plugins"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

const (
	maxArchiveBytes = int64(512 << 20)
	maxExtractFiles = 256
)

func (s *Supervisor) verifyOrRepairBinary(ctx context.Context, row *store.InstalledPlugin, binaryPath string) (string, error) {
	if row.BinarySHA256 != "" {
		actual, err := binaryDigest(binaryPath)
		if err == nil && actual == row.BinarySHA256 {
			return binaryPath, nil
		}
	}
	entry := plugins.CatalogEntry{ID: row.ID, Version: row.Version}
	asset := plugins.Asset{URL: row.AssetURL, SHA256: row.ArchiveSHA256}
	manifestData, binaryPath, err := s.downloadAndExtract(ctx, entry, asset, s.versionDir(entry))
	if err != nil {
		return "", err
	}
	if err := validateArchiveManifest(manifestData, entry); err != nil {
		return "", err
	}
	if string(manifestData) != row.ManifestJSON {
		return "", errors.New("plugin archive manifest does not match installed manifest")
	}
	actual, err := binaryDigest(binaryPath)
	if err != nil {
		return "", err
	}
	if row.BinarySHA256 != "" && actual != row.BinarySHA256 {
		return "", fmt.Errorf("plugin binary SHA-256 mismatch after re-download: got %s", actual)
	}
	if row.BinarySHA256 == "" {
		row.BinarySHA256 = actual
		if err := s.opts.Store.SetInstalledPlugin(ctx, row); err != nil {
			return "", fmt.Errorf("save verified plugin binary SHA-256: %w", err)
		}
	}
	return binaryPath, nil
}

func binaryDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("inspect plugin binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("plugin binary is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open plugin binary: %w", err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash plugin binary: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Supervisor) downloadAndExtract(ctx context.Context, entry plugins.CatalogEntry, asset plugins.Asset, versionDir string) ([]byte, string, error) {
	if isZeroDigest(asset.SHA256) {
		return nil, "", errors.New("catalog entry not released yet")
	}
	if err := os.MkdirAll(filepath.Dir(versionDir), 0o700); err != nil {
		return nil, "", fmt.Errorf("create plugin directory: %w", err)
	}
	archiveFile, err := os.CreateTemp(filepath.Dir(versionDir), ".plugin-download-*.tar.gz")
	if err != nil {
		return nil, "", fmt.Errorf("create plugin archive: %w", err)
	}
	archivePath := archiveFile.Name()
	defer func() {
		_ = archiveFile.Close()
		_ = os.Remove(archivePath)
	}()

	resp, err := s.getAsset(ctx, asset.URL)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download plugin asset: HTTP %d", resp.StatusCode)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(archiveFile, hash), io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("download plugin asset: %w", err)
	}
	if written > maxArchiveBytes {
		return nil, "", fmt.Errorf("plugin archive exceeds %d bytes", maxArchiveBytes)
	}
	if err := archiveFile.Sync(); err != nil {
		return nil, "", fmt.Errorf("sync plugin archive: %w", err)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != asset.SHA256 {
		return nil, "", fmt.Errorf("plugin archive SHA-256 mismatch: got %s", got)
	}
	if err := archiveFile.Close(); err != nil {
		return nil, "", fmt.Errorf("close plugin archive: %w", err)
	}

	stage, err := os.MkdirTemp(filepath.Dir(versionDir), "."+entry.ID+"-stage-")
	if err != nil {
		return nil, "", fmt.Errorf("create plugin staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := extractTarGz(archivePath, stage); err != nil {
		return nil, "", err
	}
	manifestPath := filepath.Join(stage, "manifest.json")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, "", fmt.Errorf("plugin archive is missing manifest.json: %w", err)
	}
	binaryPath := filepath.Join(stage, binaryName(entry.ID))
	info, err := os.Stat(binaryPath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("plugin archive is missing regular binary %s", binaryName(entry.ID))
	}
	if err := os.RemoveAll(versionDir); err != nil {
		return nil, "", fmt.Errorf("replace previous plugin files: %w", err)
	}
	if err := os.Rename(stage, versionDir); err != nil {
		return nil, "", fmt.Errorf("install plugin files: %w", err)
	}
	return manifestData, filepath.Join(versionDir, binaryName(entry.ID)), nil
}

func (s *Supervisor) getAsset(ctx context.Context, rawURL string) (*http.Response, error) {
	current, err := url.Parse(rawURL)
	if err != nil || (current.Scheme != "http" && current.Scheme != "https") ||
		current.Host == "" || current.User != nil || current.Fragment != "" {
		return nil, errors.New("plugin asset URL must be absolute HTTP or HTTPS")
	}
	for redirects := 0; redirects <= 5; redirects++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build plugin download request: %w", err)
		}
		response, err := s.opts.HTTPClient.Do(request)
		if err != nil {
			return nil, fmt.Errorf("download plugin asset: %w", err)
		}
		if response.StatusCode < 300 || response.StatusCode > 399 {
			return response, nil
		}
		location := response.Header.Get("Location")
		_ = response.Body.Close()
		if location == "" {
			return nil, fmt.Errorf("plugin asset redirect has no location (HTTP %d)", response.StatusCode)
		}
		if redirects == 5 {
			return nil, errors.New("plugin asset exceeded redirect limit")
		}
		next, err := current.Parse(location)
		if err != nil || next.Host == "" || next.User != nil || next.Fragment != "" ||
			(next.Scheme != "http" && next.Scheme != "https") {
			return nil, errors.New("plugin asset redirect URL is invalid")
		}
		if current.Scheme == "https" && next.Scheme != "https" {
			return nil, errors.New("plugin asset redirect cannot downgrade HTTPS")
		}
		current = next
	}
	return nil, errors.New("plugin asset exceeded redirect limit")
}

func extractTarGz(archivePath, destination string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open plugin archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open plugin gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	var total int64
	files := 0
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read plugin tar archive: %w", err)
		}
		name, err := safeArchiveName(header.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create plugin archive directory: %w", err)
			}
		case tar.TypeReg:
			files++
			if files > maxExtractFiles {
				return fmt.Errorf("plugin archive contains more than %d files", maxExtractFiles)
			}
			if header.Size < 0 || header.Size > maxArchiveBytes-total {
				return errors.New("plugin archive exceeds extraction size limit")
			}
			total += header.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create plugin archive parent directory: %w", err)
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return fmt.Errorf("create plugin archive file %s: %w", name, err)
			}
			written, copyErr := io.CopyN(out, reader, header.Size)
			closeErr := out.Close()
			if copyErr != nil || written != header.Size {
				return fmt.Errorf("extract plugin archive file %s: %w", name, errors.Join(copyErr, closeErr))
			}
			if closeErr != nil {
				return fmt.Errorf("close plugin archive file %s: %w", name, closeErr)
			}
		default:
			return fmt.Errorf("plugin archive contains unsupported entry type for %s", name)
		}
	}
}

func safeArchiveName(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `:\`) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("plugin archive contains unsafe path %q", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return "", fmt.Errorf("plugin archive contains unsafe path %q", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") {
		return "", fmt.Errorf("plugin archive contains unsafe path %q", name)
	}
	return clean, nil
}

func binaryName(id string) string {
	name := "varde-plugin-" + id
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func validateArchiveManifest(data []byte, entry plugins.CatalogEntry) error {
	plugin, err := plugins.NewWithSecret(data, "http://127.0.0.1:1", "plugin-"+entry.ID, []byte(strings.Repeat("x", 32)))
	if err != nil {
		return fmt.Errorf("invalid plugin manifest: %w", err)
	}
	if plugin.ID != entry.ID {
		return fmt.Errorf("archive manifest id %q does not match catalog id %q", plugin.ID, entry.ID)
	}
	if strings.TrimPrefix(plugin.Version, "v") != strings.TrimPrefix(entry.Version, "v") {
		return fmt.Errorf("archive manifest version %q does not match catalog version %q", plugin.Version, entry.Version)
	}
	return nil
}
