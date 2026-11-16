package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCompareReleaseTags(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"v1.4", "1.4.0", 0},
		{"v1.4.1", "1.4.0", 1},
		{"v1.5", "v1.4.99", 1},
		{"v1.4.0", "v2.0", -1},
		{"v1.4.1", "v1.4.1", 0},
		{"v1.4.0.2", "v1.4", 1},
	} {
		got, err := compareReleaseTags(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Errorf("compareReleaseTags(%q, %q) = %d, %v, want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
	for _, tag := range []string{"", "../../v2.0", "beta", "v1.5-rc.1", "v1.4;echo hi"} {
		if _, err := compareReleaseTags(tag, "1.4.0"); err == nil {
			t.Errorf("accepted unsafe/prerelease tag %q", tag)
		}
	}
}

func fakeRelease(tag, filename, digest string) releaseDescriptor {
	return releaseDescriptor{
		Tag: tag,
		URL: "https://github.com/f-stuff/Mixtape/releases/tag/" + tag,
		File: releaseAsset{
			Name:   filename,
			Size:   1234,
			URL:    "https://github.com/f-stuff/Mixtape/releases/download/" + tag + "/" + filename,
			Digest: "sha256:" + digest,
		},
	}
}

func TestReleaseURLIsRestrictedToOwnRepository(t *testing.T) {
	asset := fakeRelease("v1.4.1", "Mixtape-Complete-Portable-Windows.zip", strings.Repeat("a", 64))
	if !validReleaseAssetURL(asset.File.URL, asset.Tag, asset.File.Name) {
		t.Fatal("rejected proper release asset")
	}
	for _, u := range []string{
		"http://github.com/f-stuff/Mixtape/releases/download/v1.4.1/Mixtape-Complete-Portable-Windows.zip",
		"https://github.com.evil.test/f-stuff/Mixtape/releases/download/v1.4.1/Mixtape-Complete-Portable-Windows.zip",
		"https://evil.test/file.zip",
		"https://github.com/other/Other/releases/download/v1.4.1/Mixtape-Complete-Portable-Windows.zip",
		"https://attacker@github.com/f-stuff/Mixtape/releases/download/v1.4.1/Mixtape-Complete-Portable-Windows.zip",
	} {
		if validReleaseAssetURL(u, asset.Tag, asset.File.Name) {
			t.Errorf("accepted unsafe download URL %s", u)
		}
	}
}

func TestGitHubReleaseAPISelectsSupportedZip(t *testing.T) {
	payload := map[string]any{
		"tag_name": "v1.4.1", "html_url": "https://github.com/f-stuff/Mixtape/releases/tag/v1.4.1",
		"assets": []releaseAsset{
			fakeRelease("v1.4.1", "Mixtape-Portable-Windows.zip", strings.Repeat("a", 64)).File,
			fakeRelease("v1.4.1", "Mixtape-Complete-Portable-Windows.zip", strings.Repeat("b", 64)).File,
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Error("missing GitHub API content type")
		}
		json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()
	descriptor, err := fetchReleaseFromAPI(context.Background(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Tag != "v1.4.1" || descriptor.File.Name != "Mixtape-Complete-Portable-Windows.zip" {
		t.Fatalf("unexpected release: %+v", descriptor)
	}
}

func TestReleaseCheckNoUpdateAndNewer(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.releaseFetcher = func(ctx context.Context) (releaseDescriptor, error) {
		return fakeRelease("v1.4", "Mixtape-Portable-Windows.zip", strings.Repeat("a", 64)), nil
	}
	if err := a.checkReleaseUpdates(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if a.releaseUpdate.State != "current" || a.releaseUpdate.CanInstall {
		t.Fatalf("same version shouldn't update: %+v", a.releaseUpdate)
	}
	a.releaseFetcher = func(ctx context.Context) (releaseDescriptor, error) {
		return fakeRelease("v1.4.2", "Mixtape-Portable-Windows.zip", strings.Repeat("b", 64)), nil
	}
	if err := a.checkReleaseUpdates(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if a.releaseUpdate.State != "available" || !a.releaseUpdate.CanInstall || a.releaseUpdate.Latest != "v1.4.2" {
		t.Fatalf("newer release wasn't offered: %+v", a.releaseUpdate)
	}
	a.releaseFetcher = func(ctx context.Context) (releaseDescriptor, error) {
		return fakeRelease("v1.4.3", "Mixtape-Portable-Windows.zip", ""), nil
	}
	if err := a.checkReleaseUpdates(context.Background(), true); err == nil {
		t.Fatal("release without a digest must be rejected")
	}
	if a.releaseUpdate.CanInstall {
		t.Fatal("missing digest should prevent installation")
	}
}

func TestUpdateIsBlockedWithQueuedSongs(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.releaseAsset = fakeRelease("v1.4.1", "Mixtape-Portable-Windows.zip", strings.Repeat("a", 64))
	a.addJobs([]*Job{{Kind: "url", Key: "url:a", Status: "queued", URL: "https://www.youtube.com/watch?v=12345678900"}})
	if err := a.beginReleaseUpdate(); err == nil {
		t.Fatal("an update may not interrupt queued jobs")
	}
	a.mu.Lock()
	a.state.Paused = true
	a.mu.Unlock()
	// Do not invoke beginReleaseUpdate here: it would launch asynchronous work.
	if a.pendingDownloadsLockedNoTest() {
		t.Fatal("paused queue should not be treated as active for updating")
	}
}

// Test convenience wrapper: always acquire the lock just like production.
func (a *App) pendingDownloadsLockedNoTest() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pendingDownloadsLocked()
}

func createTestReleaseZIP(t *testing.T, contents map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	zw := zip.NewWriter(&output)
	for path, data := range contents {
		f, err := zw.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func fakePE() []byte {
	return append([]byte{'M', 'Z'}, bytes.Repeat([]byte{0}, 72*1024)...)
}

func TestSafeZipExtractionAndMissingFiles(t *testing.T) {
	root := t.TempDir()
	zipped := createTestReleaseZIP(t, map[string][]byte{
		"Mixtape.exe":      fakePE(),
		"README.md":        []byte("Mixtape"),
		"queue.json":       []byte(`{"jobs": []}`), // must never overwrite a library
		"tools/yt-dlp.exe": []byte("tool binary"),
	})
	zipPath := filepath.Join(root, "release.zip")
	if err := os.WriteFile(zipPath, zipped, 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "stage")
	if err := extractStagedRelease(zipPath, stage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, "Mixtape.exe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, "queue.json")); !os.IsNotExist(err) {
		t.Fatal("user queue data must never be copied out of release archive")
	}

	for _, path := range []string{"../escape.txt", "tools/../../escape.txt", "C:/escape.txt", `tools\evil.exe`} {
		zipPath := filepath.Join(root, "unsafe.zip")
		bad := createTestReleaseZIP(t, map[string][]byte{"Mixtape.exe": fakePE(), path: []byte("malicious")})
		if err := os.WriteFile(zipPath, bad, 0600); err != nil {
			t.Fatal(err)
		}
		if err := extractStagedRelease(zipPath, filepath.Join(root, "untrusted")); err == nil {
			t.Errorf("accepted unsafe ZIP entry %s", path)
		}
	}
	badOnly := filepath.Join(root, "noexe.zip")
	_ = os.WriteFile(badOnly, createTestReleaseZIP(t, map[string][]byte{"README.md": []byte("not an app")}), 0600)
	if err := extractStagedRelease(badOnly, filepath.Join(root, "noexe")); err == nil {
		t.Error("accepted release with no Mixtape.exe")
	}
}

func TestVerifiedZIPDownloadRejectsTampering(t *testing.T) {
	payload := createTestReleaseZIP(t, map[string][]byte{"Mixtape.exe": fakePE()})
	digest := sha256.Sum256(payload)
	recv := make(chan int, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	}))
	defer server.Close()
	asset := releaseAsset{URL: server.URL, Name: "Mixtape-Portable-Windows.zip", Size: int64(len(payload)), Digest: "sha256:" + hex.EncodeToString(digest[:])}
	if err := downloadVerifiedZIP(context.Background(), server.Client(), asset, filepath.Join(t.TempDir(), "valid.zip"), func(i int) {
		select {
		case recv <- i:
		default:
		}
	}); err != nil {
		t.Fatal(err)
	}
	asset.Digest = "sha256:" + strings.Repeat("0", 64)
	if err := downloadVerifiedZIP(context.Background(), server.Client(), asset, filepath.Join(t.TempDir(), "altered.zip"), nil); err == nil {
		t.Fatal("accepted digest mismatch")
	}
	asset.Digest = "sha256:" + hex.EncodeToString(digest[:])
	asset.Size += 10
	if err := downloadVerifiedZIP(context.Background(), server.Client(), asset, filepath.Join(t.TempDir(), "truncated.zip"), nil); err == nil {
		t.Fatal("accepted incomplete download")
	}
}

func TestStagedUpdateLaunchAndShutdown(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := createTestReleaseZIP(t, map[string][]byte{
		"Mixtape.exe": fakePE(), "README.md": []byte("new version"),
	})
	digest := sha256.Sum256(payload)
	release := fakeRelease("v1.4.1", "Mixtape-Portable-Windows.zip", hex.EncodeToString(digest[:]))
	release.File.Size = int64(len(payload))
	// Actual downloader is tested independently. A fake only places an archive
	// into the user's LOCALAPPDATA staging area; the real extractor still runs.
	a.releaseDownloader = func(ctx context.Context, client *http.Client, asset releaseAsset, output string, cb func(int)) error {
		return os.WriteFile(output, payload, 0600)
	}
	called := make(chan struct{}, 1)
	a.releaseLauncher = func(script, installDir, stageDir, dataDir, tag string, pid int) error {
		if filepath.Base(script) != "apply-update.ps1" || tag != "v1.4.1" || pid <= 0 {
			t.Errorf("wrong installer arguments: %s, %s, %d", script, tag, pid)
		}
		if _, err := os.Stat(filepath.Join(stageDir, "payload", "Mixtape.exe")); err != nil {
			t.Errorf("missing staged exe: %v", err)
		}
		if _, err := os.Stat(script); err != nil {
			t.Errorf("missing helper script: %v", err)
		}
		called <- struct{}{}
		return nil
	}
	shut := make(chan struct{}, 1)
	a.shutdown = func() { shut <- struct{}{} }
	a.downloadAndInstallRelease(release)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("did not launch updater")
	}
	select {
	case <-shut:
	case <-time.After(time.Second):
		t.Fatal("did not shut down after updater started")
	}
}

func TestEffectiveVersionMarkerMustMatchExecutable(t *testing.T) {
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sha := sha256.New()
	if _, err := io.Copy(sha, f); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, "app-release.json")
	marker, _ := json.Marshal(map[string]string{"tag": "v1.4.2", "exeSHA256": hex.EncodeToString(sha.Sum(nil))})
	if err := os.WriteFile(markerPath, marker, 0600); err != nil {
		t.Fatal(err)
	}
	if v := installedReleaseVersion(root); v != "v1.4.2" {
		t.Errorf("expected installed version from marker, got %s", v)
	}
	if err := os.WriteFile(markerPath, []byte(`{"tag":"v9.9","exeSHA256":"`+strings.Repeat("a", 64)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if v := installedReleaseVersion(root); v != appVersion {
		t.Errorf("tampered marker should fall back to compiled app version, got %s", v)
	}
}
