package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This updater is intentionally separate from the weekly yt-dlp updater.
// It only checks published releases of the owner's repository and never
// replaces a running EXE. The actual file swap is performed after exit.
const (
	releaseAPIURL           = "https://api.github.com/repos/f-stuff/Mixtape/releases/latest"
	releasePageURL          = "https://github.com/f-stuff/Mixtape/releases/latest"
	releaseCheckInterval    = 24 * time.Hour
	releaseFailedCheckDelay = 4 * time.Hour
	maxReleaseZIPBytes      = 750 << 20
	maxUnpackedReleaseBytes = 2 << 30
)

var releaseTagPattern = regexp.MustCompile(`^[vV]?\d+\.\d+(?:\.\d+){0,2}$`)
var releaseSHA256Pattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// ReleaseUpdateStatus is shown in Settings and in a small update-available
// banner. State: checking, current, available, downloading, installing, error.
type ReleaseUpdateStatus struct {
	State         string    `json:"state"`
	Current       string    `json:"current"`
	Latest        string    `json:"latest,omitempty"`
	PageURL       string    `json:"pageUrl,omitempty"`
	Message       string    `json:"message,omitempty"`
	Progress      int       `json:"progress,omitempty"`
	CanInstall    bool      `json:"canInstall,omitempty"`
	InstallError  string    `json:"installError,omitempty"`
	LastCheckedAt time.Time `json:"lastCheckedAt,omitempty"`
}

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type releaseDescriptor struct {
	Tag  string
	URL  string
	File releaseAsset
}

// Comparing numeric stable tags (v1.4 == v1.4.0) avoids treating an
// already-installed ZIP as a new release every time Mixtape launches.
func compareReleaseTags(a, b string) (int, error) {
	parse := func(s string) ([]uint64, error) {
		if !releaseTagPattern.MatchString(s) {
			return nil, fmt.Errorf("unsupported release tag %q (use v1.4.1 or v1.5)", s)
		}
		s = strings.TrimLeft(s, "vV")
		parts := strings.Split(s, ".")
		version := make([]uint64, 4)
		for i, p := range parts {
			n, err := strconv.ParseUint(p, 10, 32)
			if err != nil {
				return nil, err
			}
			version[i] = n
		}
		return version, nil
	}
	x, err := parse(a)
	if err != nil {
		return 0, err
	}
	y, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] > y[i] {
			return 1, nil
		}
		if x[i] < y[i] {
			return -1, nil
		}
	}
	return 0, nil
}

func validReleaseAssetURL(raw, tag, name string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.EscapedPath() == "/f-stuff/Mixtape/releases/download/"+url.PathEscape(tag)+"/"+url.PathEscape(name)
}

func fetchLatestMixtapeRelease(ctx context.Context) (releaseDescriptor, error) {
	client := &http.Client{Timeout: 18 * time.Second}
	return fetchReleaseFromAPI(ctx, client, releaseAPIURL)
}

func fetchReleaseFromAPI(ctx context.Context, client *http.Client, api string) (releaseDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return releaseDescriptor{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Mixtape-Portable-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return releaseDescriptor{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			return releaseDescriptor{}, errors.New("GitHub releases are unavailable. Make sure f-stuff/Mixtape is public and has a published release")
		}
		return releaseDescriptor{}, fmt.Errorf("GitHub release check returned HTTP %d", resp.StatusCode)
	}
	var latest struct {
		Tag        string         `json:"tag_name"`
		URL        string         `json:"html_url"`
		Draft      bool           `json:"draft"`
		Prerelease bool           `json:"prerelease"`
		Assets     []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&latest); err != nil {
		return releaseDescriptor{}, fmt.Errorf("invalid GitHub release response: %w", err)
	}
	if latest.Draft || latest.Prerelease || !releaseTagPattern.MatchString(latest.Tag) {
		return releaseDescriptor{}, errors.New("latest release needs a stable version tag such as v1.4.1")
	}
	if latest.URL != "" && !strings.HasPrefix(latest.URL, "https://github.com/f-stuff/Mixtape/releases/") {
		return releaseDescriptor{}, errors.New("unexpected GitHub release address")
	}
	result := releaseDescriptor{Tag: latest.Tag, URL: latest.URL}
	// Prefer the complete ZIP if both distribution variants are published.
	for _, preferred := range []string{"Mixtape-Complete-Portable-Windows.zip", "Mixtape-Portable-Windows.zip"} {
		for _, asset := range latest.Assets {
			if asset.Name == preferred && validReleaseAssetURL(asset.URL, latest.Tag, preferred) && asset.Size > 0 && asset.Size <= maxReleaseZIPBytes {
				result.File = asset
				return result, nil
			}
		}
	}
	return result, errors.New("latest release has no supported Mixtape portable ZIP attached")
}

func (a *App) setReleaseStatus(state, message string, progress int) {
	a.mu.Lock()
	a.releaseUpdate.State = state
	a.releaseUpdate.Message = message
	a.releaseUpdate.Progress = progress
	a.mu.Unlock()
}

func (a *App) checkReleaseUpdates(ctx context.Context, force bool) error {
	a.releaseCheckMu.Lock()
	defer a.releaseCheckMu.Unlock()
	a.mu.Lock()
	if a.releaseInstalling {
		a.mu.Unlock()
		return errors.New("an update is already being installed")
	}
	if !force && !a.releaseUpdate.LastCheckedAt.IsZero() {
		interval := releaseCheckInterval
		if a.releaseUpdate.State == "error" {
			interval = releaseFailedCheckDelay
		}
		if time.Since(a.releaseUpdate.LastCheckedAt) < interval {
			a.mu.Unlock()
			return nil
		}
	}
	a.releaseUpdate.State = "checking"
	a.releaseUpdate.CanInstall = false
	a.releaseAsset = releaseDescriptor{}
	a.releaseUpdate.Message = "Checking GitHub Releases…"
	a.mu.Unlock()

	lookup := a.releaseFetcher
	if lookup == nil {
		lookup = fetchLatestMixtapeRelease
	}
	release, err := lookup(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.releaseUpdate.LastCheckedAt = time.Now()
	if err != nil {
		a.releaseUpdate.State = "error"
		a.releaseUpdate.Message = err.Error()
		return err
	}
	cmp, err := compareReleaseTags(release.Tag, a.currentVersion)
	if err != nil {
		a.releaseUpdate.State = "error"
		a.releaseUpdate.Message = err.Error()
		return err
	}
	a.releaseUpdate.Latest = release.Tag
	a.releaseUpdate.PageURL = release.URL
	if cmp <= 0 {
		a.releaseAsset = releaseDescriptor{}
		a.releaseUpdate.State = "current"
		a.releaseUpdate.Message = "Mixtape is up to date"
		return nil
	}
	if !strings.HasPrefix(release.File.Digest, "sha256:") || !releaseSHA256Pattern.MatchString(strings.TrimPrefix(release.File.Digest, "sha256:")) {
		err = errors.New("GitHub did not provide a SHA-256 checksum for this release asset; install it manually instead")
		a.releaseUpdate.State = "error"
		a.releaseUpdate.Message = err.Error()
		return err
	}
	a.releaseAsset = release
	a.releaseUpdate.State = "available"
	a.releaseUpdate.CanInstall = true
	a.releaseUpdate.Message = release.Tag + " is available"
	return nil
}

func (a *App) releaseUpdateLoop() {
	// Checks happen quietly at startup and every day while the app stays open.
	// The check does not interrupt downloads; installing requires an idle queue.
	_ = a.checkReleaseUpdates(context.Background(), false)
	tick := time.NewTicker(30 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-a.stopping:
			return
		case <-tick.C:
			_ = a.checkReleaseUpdates(context.Background(), false)
		}
	}
}

func (a *App) beginReleaseUpdate() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.releaseInstalling {
		return errors.New("update is already running")
	}
	if a.releaseAsset.File.URL == "" {
		return errors.New("no new release found; check for updates first")
	}
	// Never interrupt a selected song, a tool operation or queued jobs.
	// These are kept exactly as they were if the user closes the page.
	if a.activeID != "" || a.pendingDownloadsLocked() || a.setupRunning || a.updaterBusy {
		return errors.New("finish or pause the download queue before updating Mixtape")
	}
	a.releaseInstalling = true
	a.releaseUpdate.State = "downloading"
	a.releaseUpdate.Message = "Downloading " + a.releaseAsset.Tag + "…"
	a.releaseUpdate.Progress = 0
	release := a.releaseAsset
	go a.downloadAndInstallRelease(release)
	return nil
}

func githubDownloadClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("unsafe release download redirect")
			}
			host := strings.ToLower(req.URL.Hostname())
			if host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com" {
				return errors.New("release download redirected to an unexpected host")
			}
			return nil
		},
	}
}

// The ZIP is never executed without checking the SHA-256 digest supplied by
// GitHub's API. A size limit and an allowlist of paths prevent zip-slip and
// overwriting the user's queue or music collection.
func downloadVerifiedZIP(ctx context.Context, client *http.Client, asset releaseAsset, file string, progress func(int)) error {
	expected := strings.TrimPrefix(asset.Digest, "sha256:")
	if !strings.HasPrefix(asset.Digest, "sha256:") || !releaseSHA256Pattern.MatchString(expected) {
		return errors.New("GitHub release has no verified SHA-256 checksum")
	}
	if asset.Size < 1 || asset.Size > maxReleaseZIPBytes {
		return errors.New("release ZIP size is outside the supported range")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mixtape-Portable-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release download returned HTTP %d", resp.StatusCode)
	}
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	hash := sha256.New()
	copied := int64(0)
	buffer := make([]byte, 256*1024)
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			copied += int64(n)
			if copied > asset.Size || copied > maxReleaseZIPBytes {
				return errors.New("release ZIP exceeds its announced size")
			}
			if _, err := out.Write(buffer[:n]); err != nil {
				return err
			}
			_, _ = hash.Write(buffer[:n])
			if progress != nil {
				progress(int(100 * copied / asset.Size))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if copied != asset.Size {
		return fmt.Errorf("incomplete release ZIP: received %d of %d bytes", copied, asset.Size)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expected) {
		return errors.New("downloaded ZIP does not match GitHub's SHA-256 checksum")
	}
	return out.Sync()
}

var allowedUpdateFiles = map[string]bool{
	"Mixtape.exe":       true,
	"README.md":         true,
	"QUICK START.txt":   true,
	"THIRD_PARTY.md":    true,
	"tools/yt-dlp.exe":  true,
	"tools/ffmpeg.exe":  true,
	"tools/ffprobe.exe": true,
	"tools/deno.exe":    true,
}

func extractStagedRelease(zipFile, target string) error {
	z, err := zip.OpenReader(zipFile)
	if err != nil {
		return fmt.Errorf("release is not a valid ZIP: %w", err)
	}
	defer z.Close()
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	seen := make(map[string]bool)
	total := uint64(0)
	foundExecutable := false
	for _, f := range z.File {
		name := f.Name
		// Reject dangerous entries, even ones we otherwise wouldn't extract.
		if strings.ContainsAny(name, `\:`) || strings.HasPrefix(name, "/") || path.Clean(name) == ".." || strings.HasPrefix(path.Clean(name), "../") || f.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe file path in release archive")
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if seen[strings.ToLower(name)] {
			return errors.New("duplicate file in release archive")
		}
		seen[strings.ToLower(name)] = true
		if !allowedUpdateFiles[name] {
			continue // Never overwrite user files or unrecognised application files.
		}
		total += f.UncompressedSize64
		if f.UncompressedSize64 > 600<<20 || total > maxUnpackedReleaseBytes {
			return errors.New("release has unexpectedly large unpacked files")
		}
		output := filepath.Join(target, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
			return err
		}
		reader, err := f.Open()
		if err != nil {
			return err
		}
		file, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			reader.Close()
			return err
		}
		n, err := io.CopyN(file, reader, int64(f.UncompressedSize64))
		closeFileErr := file.Close()
		_ = reader.Close()
		if err != nil || closeFileErr != nil || uint64(n) != f.UncompressedSize64 {
			return fmt.Errorf("cannot unpack %s: %v %v", name, err, closeFileErr)
		}
		if name == "Mixtape.exe" {
			bin, err := os.Open(output)
			if err != nil {
				return err
			}
			var magic [2]byte
			_, err = io.ReadFull(bin, magic[:])
			_ = bin.Close()
			if err != nil || string(magic[:]) != "MZ" || f.UncompressedSize64 < 64*1024 {
				return errors.New("release does not contain a valid-looking Windows Mixtape.exe")
			}
			foundExecutable = true
		}
	}
	if !foundExecutable {
		return errors.New("release ZIP contains no Mixtape.exe in its root folder")
	}
	return nil
}

func (a *App) downloadAndInstallRelease(release releaseDescriptor) {
	fail := func(err error) {
		a.mu.Lock()
		a.releaseInstalling = false
		a.releaseUpdate.State = "error"
		a.releaseUpdate.CanInstall = true
		a.releaseUpdate.Message = "Update failed: " + err.Error()
		a.releaseUpdate.Progress = 0
		a.mu.Unlock()
	}
	updatesPath := filepath.Join(a.dataDir, "updates")
	if err := os.MkdirAll(updatesPath, 0700); err != nil {
		fail(err)
		return
	}
	stage, err := os.MkdirTemp(updatesPath, "mixtape-release-")
	if err != nil {
		fail(err)
		return
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(stage)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	asset := release.File
	if !validReleaseAssetURL(asset.URL, release.Tag, asset.Name) {
		fail(errors.New("invalid GitHub asset address"))
		return
	}
	lastPercent := -1
	downloader := a.releaseDownloader
	if downloader == nil {
		downloader = downloadVerifiedZIP
	}
	err = downloader(ctx, githubDownloadClient(), asset, filepath.Join(stage, "release.zip"), func(p int) {
		if p != lastPercent {
			lastPercent = p
			a.setReleaseStatus("downloading", fmt.Sprintf("Downloading %s: %d%%", release.Tag, p), p)
		}
	})
	if err != nil {
		fail(err)
		return
	}
	a.setReleaseStatus("installing", "Preparing update…", 100)
	payload := filepath.Join(stage, "payload")
	if err := extractStagedRelease(filepath.Join(stage, "release.zip"), payload); err != nil {
		fail(err)
		return
	}
	// PowerShell is included in Windows 10/11. It waits for our process to
	// exit, swaps just the application files, and launches the new Mixtape.
	script := filepath.Join(stage, "apply-update.ps1")
	if err := os.WriteFile(script, releaseInstallerScript, 0600); err != nil {
		fail(err)
		return
	}
	appExe, err := os.Executable()
	if err != nil {
		fail(err)
		return
	}
	helper := a.releaseLauncher
	if helper == nil {
		helper = startReleaseInstaller
	}
	if err := helper(script, filepath.Dir(appExe), stage, a.dataDir, release.Tag, os.Getpid()); err != nil {
		fail(err)
		return
	}
	// From here the separate updater owns the staging directory. It cannot
	// replace Mixtape.exe until this process has fully stopped.
	cleanup = false
	a.setReleaseStatus("installing", "Restarting Mixtape…", 100)
	if a.shutdown != nil {
		a.shutdown()
	}
}

// For ZIPs installed by the updater, remember the release tag alongside the
// SHA-256 of the EXE. This prevents update loops even if someone forgets to
// change the compiled version number before publishing a new release.
func installedReleaseVersion(dataDir string) string {
	current := appVersion
	b, err := os.ReadFile(filepath.Join(dataDir, "app-release.json"))
	if err != nil {
		return current
	}
	var marker struct {
		Tag       string `json:"tag"`
		ExeSHA256 string `json:"exeSHA256"`
	}
	if json.Unmarshal(b, &marker) != nil || !releaseTagPattern.MatchString(marker.Tag) || !releaseSHA256Pattern.MatchString(marker.ExeSHA256) {
		return current
	}
	exe, err := os.Executable()
	if err != nil {
		return current
	}
	file, err := os.Open(exe)
	if err != nil {
		return current
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || !strings.EqualFold(marker.ExeSHA256, hex.EncodeToString(hash.Sum(nil))) {
		return current
	}
	if cmp, err := compareReleaseTags(marker.Tag, current); err == nil && cmp > 0 {
		return marker.Tag
	}
	return current
}
