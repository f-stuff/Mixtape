package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (a *App) handleJob(ctx context.Context, j Job) error {
	switch j.Kind {
	case "album":
		return a.prepareAlbum(ctx, j)
	case "track":
		return a.downloadTrack(ctx, j)
	case "url":
		return a.downloadURL(ctx, j)
	default:
		return errors.New("Unknown job type")
	}
}
func (a *App) prepareAlbum(ctx context.Context, j Job) error {
	a.changeJob(j.ID, func(v *Job) { v.Status = "preparing"; v.Message = "Looking up the official tracklist…" })
	songs, err := a.getAlbumTracks(ctx, j.AlbumID, j.Album, j.AlbumDate)
	if err != nil {
		return err
	}
	a.mu.Lock()
	dir := albumDir(a.state.Settings.OutputDir, j)
	a.mu.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Album art is optional and fetched without holding up the next album.
	go a.tryCover(context.Background(), j.AlbumID, dir)
	jobs := make([]*Job, 0, len(songs))
	for _, t := range songs {
		key := fmt.Sprintf("track:%s:%s:%s", j.AlbumID, t.Number, normalize(t.Title))
		jobs = append(jobs, &Job{Key: key, Kind: "track", ParentID: j.ID, Artist: j.Artist, Album: j.Album,
			AlbumID: j.AlbumID, AlbumDate: j.AlbumDate, Title: t.Title, Number: t.Number, Disc: t.Disc,
			DurationMS: t.DurationMS, Message: "Waiting for a YouTube match"})
	}
	added := a.addJobs(jobs)
	a.changeJob(j.ID, func(v *Job) {
		v.Status = "done"
		v.Progress = 100
		v.Output = dir
		v.Message = fmt.Sprintf("Tracklist ready · %d tracks, %d added", len(songs), added)
	})
	return nil
}

func (a *App) tryCover(ctx context.Context, albumID, dir string) {
	if !validUUID(albumID) {
		return
	}
	p := filepath.Join(dir, "cover.jpg")
	if info, err := os.Stat(p); err == nil && info.Size() > 200 {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 14*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, "GET", "https://coverartarchive.org/release-group/"+albumID+"/front-500", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "Mixtape/1.0.0")
	response, err := a.client.Do(req)
	if err != nil {
		return
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "image/") {
		return
	}
	tmp := p + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	n, err := io.Copy(f, io.LimitReader(response.Body, 5*1024*1024))
	_ = f.Close()
	if err != nil || n < 200 {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
	}
}

type ytSearchResult struct {
	Entries []struct {
		ID         string  `json:"id"`
		URL        string  `json:"url"`
		WebpageURL string  `json:"webpage_url"`
		Title      string  `json:"title"`
		Channel    string  `json:"channel"`
		Uploader   string  `json:"uploader"`
		Duration   float64 `json:"duration"`
	} `json:"entries"`
}

func (a *App) ytCommon() []string {
	return []string{
		"--ignore-config",
		"--ffmpeg-location", a.toolsDir,
		"--js-runtimes", "deno:" + a.deno,
		"--remote-components", "ejs:github",
		"--socket-timeout", "25",
	}
}
func (a *App) searchYouTube(ctx context.Context, j Job) ([]Candidate, error) {
	query := j.Artist + " " + j.Title + " official audio"
	args := append(a.ytCommon(), "--skip-download", "--dump-single-json", "--flat-playlist", "--no-warnings", "--quiet", "ytsearch8:"+query)
	cmd := exec.CommandContext(ctx, a.ytDLP, args...)
	hideCommandWindow(cmd)
	output, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("YouTube search failed: %s", stringNoHuge(string(e.Stderr)))
		}
		return nil, err
	}
	var results ytSearchResult
	if err := json.Unmarshal(output, &results); err != nil {
		return nil, fmt.Errorf("Could not read YouTube search results: %w", err)
	}
	cands := []Candidate{}
	seen := map[string]bool{}
	for _, item := range results.Entries {
		url := item.WebpageURL
		if url == "" && len(item.ID) == 11 {
			url = "https://www.youtube.com/watch?v=" + item.ID
		}
		if url == "" && strings.HasPrefix(item.URL, "http") {
			url = item.URL
		}
		if url == "" || seen[url] {
			continue
		}
		if _, err := validateVideoURL(url); err != nil {
			continue
		}
		seen[url] = true
		channel := item.Channel
		if channel == "" {
			channel = item.Uploader
		}
		cands = append(cands, Candidate{Title: item.Title, Channel: channel, Duration: int(math.Round(item.Duration)), URL: url})
	}
	return rankCandidates(cands, j.Artist, j.Title, j.DurationMS), nil
}

func (a *App) downloadTrack(ctx context.Context, j Job) error {
	a.mu.Lock()
	root := a.state.Settings.OutputDir
	a.mu.Unlock()
	dir := albumDir(root, j)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	num := safePart(trackFileNumber(j.Number))
	if num == "Unknown" {
		num = "00"
	}
	base := num + " - " + safePart(j.Title)
	expected := filepath.Join(dir, base+".mp3")
	if info, err := os.Stat(expected); err == nil && info.Size() > 1024 {
		a.changeJob(j.ID, func(v *Job) { v.Status = "tagging"; v.Message = "Already downloaded; checking tags"; v.Progress = 96 })
		if err := a.tagTrack(ctx, j, expected, dir); err != nil {
			return err
		}
		a.changeJob(j.ID, func(v *Job) { v.Status = "done"; v.Progress = 100; v.Output = expected; v.Message = "Ready in library" })
		return nil
	}
	if j.URL == "" {
		a.changeJob(j.ID, func(v *Job) { v.Status = "searching"; v.Message = "Finding the right recording…"; v.Progress = 0 })
		matches, err := a.searchYouTube(ctx, j)
		if err != nil {
			return err
		}
		if len(matches) == 0 || matches[0].Score < 78 {
			a.changeJob(j.ID, func(v *Job) {
				v.Status = "review"
				v.Progress = 0
				v.Candidates = matches
				v.Message = "Pick the correct recording"
			})
			return nil
		}
		j.URL = matches[0].URL
		a.changeJob(j.ID, func(v *Job) { v.URL = j.URL; v.Candidates = matches; v.Message = "Matched: " + matches[0].Title })
	}
	template := filepath.Join(dir, base+".%(ext)s")
	path, err := a.runDownload(ctx, j, "track", template)
	if err != nil {
		return err
	}
	if path == "" {
		path = expected
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("Downloaded file could not be found: %w", err)
	}
	a.changeJob(j.ID, func(v *Job) {
		v.Status = "tagging"
		v.Message = "Adding album art and song details…"
		v.Progress = 97
	})
	if err := a.tagTrack(ctx, j, path, dir); err != nil {
		return err
	}
	a.changeJob(j.ID, func(v *Job) { v.Status = "done"; v.Progress = 100; v.Output = path; v.Message = "Ready in library" })
	return nil
}

func (a *App) downloadURL(ctx context.Context, j Job) error {
	a.mu.Lock()
	root := a.state.Settings.OutputDir
	a.mu.Unlock()
	folder := filepath.Join(root, "From Links")
	if err := os.MkdirAll(folder, 0755); err != nil {
		return err
	}
	tmpl := filepath.Join(folder, "%(title).150B [%(id)s].%(ext)s")
	path, err := a.runDownload(ctx, j, "url", tmpl)
	if err != nil {
		return err
	}
	// Direct videos can reveal the exact MP3 in Explorer. Playlist jobs
	// represent many files, so the button opens their common folder.
	output := folder
	if !strings.Contains(j.URL, "/playlist?") && path != "" {
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			output = path
		}
	}
	a.changeJob(j.ID, func(v *Job) {
		v.Status = "done"
		v.Progress = 100
		v.Output = output
		v.Message = "Saved to From Links"
	})
	return nil
}

var progressRe = regexp.MustCompile(`\[download\]\s+([0-9.]+)%`)

func (a *App) runDownload(ctx context.Context, j Job, kind, tmpl string) (string, error) {
	args := a.ytCommon()
	args = append(args, "--newline", "--retries", "5", "--fragment-retries", "5", "-f", "bestaudio/best",
		"--extract-audio", "--audio-format", "mp3", "--audio-quality", "0",
		"--embed-thumbnail", "--convert-thumbnails", "jpg", "--embed-metadata",
		"--print", "after_move:MIXTAPE_FILE:%(filepath)s", "-o", tmpl)
	if kind == "track" {
		args = append(args, "--no-playlist")
	} else {
		// A playlist URL downloads every song. A watch URL downloads one video.
		if strings.Contains(j.URL, "/playlist?") {
			args = append(args, "--yes-playlist")
		} else {
			args = append(args, "--no-playlist")
		}
	}
	args = append(args, "--", j.URL)
	cmd := exec.CommandContext(ctx, a.ytDLP, args...)
	hideCommandWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	a.changeJob(j.ID, func(v *Job) {
		v.Status = "downloading"
		v.Message = "Downloading and converting to MP3…"
		v.Progress = 2
	})
	var mu sync.Mutex
	paths := []string{}
	errorsSeen := []string{}
	stderrTail := []string{}
	lastPercent := -1
	read := func(rd io.Reader, isStderr bool) {
		sc := bufio.NewScanner(rd)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "MIXTAPE_FILE:") {
				value := strings.TrimPrefix(line, "MIXTAPE_FILE:")
				mu.Lock()
				paths = append(paths, value)
				mu.Unlock()
			}
			if match := progressRe.FindStringSubmatch(line); len(match) == 2 {
				p, e := strconv.ParseFloat(match[1], 64)
				if e == nil {
					now := int(math.Floor(p/4)) * 4
					mu.Lock()
					change := now > lastPercent
					if change {
						lastPercent = now
					}
					mu.Unlock()
					if change {
						a.changeJob(j.ID, func(v *Job) { v.Progress = math.Min(p, 94); v.Status = "downloading" })
					}
				}
			}
			if isStderr && line != "" {
				mu.Lock()
				stderrTail = append(stderrTail, trimErrorDetail(line, 500))
				if len(stderrTail) > 12 {
					stderrTail = stderrTail[len(stderrTail)-12:]
				}
				mu.Unlock()
			}
			if strings.Contains(line, "ERROR:") || strings.Contains(line, "WARNING:") {
				mu.Lock()
				errorsSeen = append(errorsSeen, trimErrorDetail(line, 500))
				if len(errorsSeen) > 12 {
					errorsSeen = errorsSeen[len(errorsSeen)-12:]
				}
				mu.Unlock()
			}
		}
		if scanErr := sc.Err(); scanErr != nil {
			mu.Lock()
			stderrTail = append(stderrTail, "Could not read downloader output: "+scanErr.Error())
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); read(stdout, false) }()
	go func() { defer wg.Done(); read(stderr, true) }()
	wg.Wait()
	err = cmd.Wait()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if len(errorsSeen) > 0 {
			return "", fmt.Errorf("yt-dlp failed (%w):\n%s", err, strings.Join(errorsSeen, "\n"))
		}
		if len(stderrTail) > 0 {
			return "", fmt.Errorf("yt-dlp failed (%w):\n%s", err, strings.Join(stderrTail, "\n"))
		}
		return "", fmt.Errorf("yt-dlp failed: %w", err)
	}
	if len(paths) > 0 {
		return paths[len(paths)-1], nil
	}
	// If YouTube was downloaded but no output marker arrived, do not claim success.
	return "", errors.New("Download ended without a completed MP3 file")
}

func (a *App) tagTrack(ctx context.Context, j Job, src, dir string) error {
	cover := filepath.Join(dir, "cover.jpg")
	tmp := src + ".tagging.mp3"
	_ = os.Remove(tmp)
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", src}
	hasCover := false
	if info, err := os.Stat(cover); err == nil && info.Size() > 100 {
		args = append(args, "-i", cover)
		hasCover = true
	}
	if hasCover {
		args = append(args, "-map", "0:a:0", "-map", "1:v:0", "-c:a", "copy", "-c:v", "mjpeg", "-disposition:v:0", "attached_pic")
	} else {
		args = append(args, "-map", "0", "-c", "copy")
	}
	args = append(args, "-id3v2_version", "3", "-metadata", "title="+j.Title, "-metadata", "artist="+j.Artist,
		"-metadata", "album="+j.Album, "-metadata", "track="+j.Number)
	if year := yearFromDate(j.AlbumDate); year != "" {
		args = append(args, "-metadata", "date="+year)
	}
	args = append(args, tmp)
	cmd := exec.CommandContext(ctx, a.ffmpeg, args...)
	hideCommandWindow(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("MP3 tagging failed: %s", stringNoHuge(string(bytes.TrimSpace(output))))
	}
	if info, err := os.Stat(tmp); err != nil || info.Size() < 1024 {
		_ = os.Remove(tmp)
		return errors.New("Tagging did not produce a valid MP3")
	}
	backup := src + ".untagged.bak"
	_ = os.Remove(backup)
	if err := os.Rename(src, backup); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, src); err != nil {
		_ = os.Rename(backup, src)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
