package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ytDLPURL    = "https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.26/yt-dlp.exe"
	ytDLPSHA256 = "f5389d3698ecde4ef06f4e6b1ce0294fe9ac1396228c8990b8d3416d6d177a7d"
	denoURL     = "https://github.com/denoland/deno/releases/download/v2.9.8/deno-x86_64-pc-windows-msvc.zip"
	denoSHA256  = "cba84b26e0df5ae7f74fc66b86a95b866872a86292ebd84fa1e959927edd93bf"
	ffmpegURL   = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
)

func existsNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 1000
}
func (a *App) allToolsReady() bool {
	return existsNonEmpty(a.ytDLP) && existsNonEmpty(a.ffmpeg) && existsNonEmpty(a.ffprobe) && existsNonEmpty(a.deno)
}
func (a *App) startSetup(force bool) {
	a.mu.Lock()
	if a.setupRunning || a.updaterBusy {
		a.mu.Unlock()
		return
	}
	if a.setup.State == "ready" && !force {
		a.mu.Unlock()
		return
	}
	a.setupRunning = true
	a.setup = SetupStatus{State: "checking", Message: "Checking bundled audio tools…"}
	a.mu.Unlock()
	go func() {
		defer func() { a.mu.Lock(); a.setupRunning = false; a.mu.Unlock() }()
		if err := a.ensureTools(); err != nil {
			a.updateSetup("error", "", stringNoHuge(err.Error()), 0)
		} else {
			a.updateSetup("ready", "", "Ready to download music", 100)
		}
	}()
}
func (a *App) ensureTools() error {
	if a.allToolsReady() {
		return nil
	}
	if err := os.MkdirAll(a.toolsDir, 0755); err != nil {
		return err
	}
	// A ZIP produced by the release workflow can ship every tool already inside ./tools.
	if !existsNonEmpty(a.ytDLP) {
		a.updateSetup("downloading", "yt-dlp", "Getting the YouTube downloader…", 0)
		if err := a.download(context.Background(), ytDLPURL, a.ytDLP, ytDLPSHA256, "yt-dlp"); err != nil {
			return fmt.Errorf("Could not get yt-dlp: %w", err)
		}
	}
	if !existsNonEmpty(a.ffmpeg) || !existsNonEmpty(a.ffprobe) {
		a.updateSetup("downloading", "FFmpeg", "Getting the MP3 audio converter…", 0)
		zipPath := filepath.Join(a.toolsDir, "ffmpeg-bundle.zip")
		if err := a.download(context.Background(), ffmpegURL, zipPath, "", "FFmpeg"); err != nil {
			return fmt.Errorf("Could not get FFmpeg: %w", err)
		}
		defer os.Remove(zipPath)
		if err := extractZip(zipPath, a.toolsDir, []string{"ffmpeg.exe", "ffprobe.exe"}); err != nil {
			return fmt.Errorf("Cannot unpack FFmpeg: %w", err)
		}
	}
	if !existsNonEmpty(a.deno) {
		a.updateSetup("downloading", "Deno", "Getting the YouTube audio helper…", 0)
		zipPath := filepath.Join(a.toolsDir, "deno.zip")
		if err := a.download(context.Background(), denoURL, zipPath, denoSHA256, "Deno"); err != nil {
			return fmt.Errorf("Could not get Deno: %w", err)
		}
		defer os.Remove(zipPath)
		if err := extractZip(zipPath, a.toolsDir, []string{"deno.exe"}); err != nil {
			return fmt.Errorf("Cannot unpack Deno: %w", err)
		}
	}
	if !a.allToolsReady() {
		return errors.New("Some audio tools are still missing. Press Retry setup")
	}
	return nil
}
func (a *App) download(ctx context.Context, downloadURL, target, wantedHash, component string) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 0}
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mixtape/1.0.0")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("download server returned HTTP %d", res.StatusCode)
	}
	temp := target + ".part"
	f, err := os.Create(temp)
	if err != nil {
		return err
	}
	hash := sha256.New()
	buf := make([]byte, 256*1024)
	var read int64
	lastUpdate := time.Time{}
	for {
		n, e := res.Body.Read(buf)
		if n > 0 {
			if _, err = f.Write(buf[:n]); err != nil {
				_ = f.Close()
				_ = os.Remove(temp)
				return err
			}
			_, _ = hash.Write(buf[:n])
			read += int64(n)
			if time.Since(lastUpdate) > 280*time.Millisecond {
				p := float64(0)
				if res.ContentLength > 0 {
					p = float64(read) * 100 / float64(res.ContentLength)
				}
				a.updateSetup("downloading", component, fmt.Sprintf("Downloading %s · %.1f MB", component, float64(read)/1048576), p)
				lastUpdate = time.Now()
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			_ = f.Close()
			_ = os.Remove(temp)
			return e
		}
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(temp)
		return err
	}
	if read < 1000 {
		_ = os.Remove(temp)
		return errors.New("download was unexpectedly small")
	}
	if wantedHash != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), wantedHash) {
		_ = os.Remove(temp)
		return errors.New("download hash mismatch; file was not trusted")
	}
	if err := os.Rename(temp, target); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
func extractZip(zipPath, dir string, wanted []string) error {
	archive, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer archive.Close()
	found := map[string]bool{}
	for _, entry := range archive.File {
		name := strings.ToLower(filepath.Base(entry.Name))
		okay := false
		for _, v := range wanted {
			if name == v {
				okay = true
				break
			}
		}
		if !okay || found[name] {
			continue
		}
		if entry.UncompressedSize64 > 200*1024*1024 {
			return errors.New("archive contains an unexpectedly large executable")
		}
		in, err := entry.Open()
		if err != nil {
			return err
		}
		output := filepath.Join(dir, name+".part")
		out, err := os.Create(output)
		if err != nil {
			_ = in.Close()
			return err
		}
		_, err = io.Copy(out, io.LimitReader(in, 200*1024*1024+1))
		_ = in.Close()
		_ = out.Close()
		if err != nil {
			_ = os.Remove(output)
			return err
		}
		dest := filepath.Join(dir, name)
		_ = os.Remove(dest)
		if err := os.Rename(output, dest); err != nil {
			return err
		}
		found[name] = true
	}
	for _, v := range wanted {
		if !found[v] {
			return fmt.Errorf("%s missing inside downloaded archive", v)
		}
	}
	return nil
}
