package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLifecycleSignalsBrowserShutdown(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.sessionID == "" || a.sessionID == other.sessionID {
		t.Fatal("Each backend process needs its own session ID")
	}

	handler := a.routes()
	state := httptest.NewRecorder()
	handler.ServeHTTP(state, httptest.NewRequest("GET", "http://"+listenAddress+"/api/state", nil))
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), a.sessionID) {
		t.Fatalf("State should identify the running backend: %s", state.Body.String())
	}

	request := httptest.NewRequest("GET", "http://"+listenAddress+"/api/lifecycle", nil)
	stream := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(stream, request)
		close(done)
	}()
	a.signalShutdown()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Lifecycle stream did not end after shutdown")
	}
	if !strings.Contains(stream.Header().Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(stream.Body.String(), "event: shutdown\ndata: closed") {
		t.Fatalf("Did not send browser shutdown event: %s", stream.Body.String())
	}
	// Multiple Quit paths should not panic by closing the signal twice.
	a.signalShutdown()

	badMethod := httptest.NewRecorder()
	handler.ServeHTTP(badMethod, httptest.NewRequest("POST", "http://"+listenAddress+"/api/lifecycle", nil))
	if badMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("Lifecycle POST returned %d, expected 405", badMethod.Code)
	}
}

func TestSafePartsAndURLValidation(t *testing.T) {
	if trackFileNumber("1") != "01" || trackFileNumber("2-3") != "2-03" {
		t.Fatal("Track number formatting failed")
	}
	cases := map[string]string{
		`AC/DC`: "AC-DC", `CON`: "_CON", `a<>:*?b`: "a-----b", `  little songs.. `: "little songs", "": "Unknown",
	}
	for input, want := range cases {
		if got := safePart(input); got != want {
			t.Errorf("safePart(%q) = %q, want %q", input, got, want)
		}
	}
	for _, link := range []string{"https://music.youtube.com/watch?v=12345678901", "https://www.youtube.com/playlist?list=PL12345", "https://youtu.be/abcdefg1234"} {
		if _, err := validateVideoURL(link); err != nil {
			t.Errorf("Valid URL rejected: %q: %v", link, err)
		}
	}
	for _, link := range []string{"https://evil.com/watch?v=abc", "javascript:alert(1)", "http://youtube.com/watch?v=abc", "https://youtube.com/@someone", "https://youtube.com:8443/watch?v=abc"} {
		if _, err := validateVideoURL(link); err == nil {
			t.Errorf("Dangerous URL accepted: %q", link)
		}
	}
}

func TestMatchRankingRejectsCoversAndLive(t *testing.T) {
	items := []Candidate{
		{Title: "Dreamer (Live At Wembley)", Channel: "The Example Band", Duration: 250},
		{Title: "Dreamer (Official Audio)", Channel: "The Example Band - Topic", Duration: 201},
		{Title: "Dreamer (Piano Cover)", Channel: "Somebody Else", Duration: 202},
	}
	got := rankCandidates(items, "The Example Band", "Dreamer", 202000)
	if got[0].Title != "Dreamer (Official Audio)" || got[0].Score < 78 {
		t.Fatalf("Unexpected best match: %#v", got)
	}
	if got[2].Score >= 78 {
		t.Errorf("Cover or live track incorrectly above confidence threshold: %#v", got)
	}
}

func TestSaveAndResume(t *testing.T) {
	dir := t.TempDir()
	a, err := newApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := a.addJobs([]*Job{{Kind: "url", Key: "url:a", Title: "A", URL: "https://youtu.be/abcdefghijk"}})
	if n != 1 {
		t.Fatalf("added %d", n)
	}
	if n := a.addJobs([]*Job{{Kind: "url", Key: "url:a", Title: "A"}}); n != 0 {
		t.Fatalf("duplicate accepted: %d", n)
	}
	id := a.state.Jobs[0].ID
	a.changeJob(id, func(j *Job) { j.Status = "downloading"; j.Progress = 51 })
	b, err := newApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.state.Jobs) != 1 || b.state.Jobs[0].Status != "queued" || b.state.Jobs[0].Progress != 0 {
		t.Fatalf("restart recovery failed: %+v", b.state.Jobs)
	}
}

func testPost(t *testing.T, handler http.Handler, path string, p any, header bool) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(p)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Host = listenAddress
	req.Header.Set("Content-Type", "application/json")
	if header {
		req.Header.Set("X-Mixtape", "1")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
func TestQueueHTTPAndCSRF(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	if rr := testPost(t, h, "/api/queue/urls", map[string]string{"text": "https://youtu.be/abcdefg1234"}, false); rr.Code != 403 {
		t.Fatalf("Missing CSRF header returned %d", rr.Code)
	}
	if rr := testPost(t, h, "/api/queue/urls", map[string]string{"text": "https://youtu.be/abcdefg1234\nhttps://www.youtube.com/playlist?list=PL1234"}, true); rr.Code != 200 {
		t.Fatalf("Enqueue returned %d: %s", rr.Code, rr.Body)
	}
	if len(a.state.Jobs) != 2 {
		t.Errorf("Expected 2 jobs, got %d", len(a.state.Jobs))
	}
	r := httptest.NewRequest("GET", "http://evil.invalid/api/state", nil)
	r.Host = "evil.invalid"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != 403 {
		t.Fatalf("DNS rebinding host accepted: %d", rr.Code)
	}
	// Preserve unfinished albums and finished tracks while clearing history.
	album := &Job{Kind: "album", Key: "album:1", Title: "Album", Status: "done"}
	a.addJobs([]*Job{album})
	parent := a.state.Jobs[2]
	a.changeJob(parent.ID, func(j *Job) { j.Status = "done" })
	a.addJobs([]*Job{{Kind: "track", Key: "track:1", ParentID: parent.ID, Title: "Finished"}, {Kind: "track", Key: "track:2", ParentID: parent.ID, Title: "Waiting"}})
	a.changeJob(a.state.Jobs[3].ID, func(j *Job) { j.Status = "done" })
	if rr := testPost(t, h, "/api/queue/clear", map[string]any{}, true); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if len(a.state.Jobs) != 5 {
		t.Fatalf("Unexpected item count after clear: %d", len(a.state.Jobs))
	}
}

func TestMusicBrainzQueries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/artist"):
			io.WriteString(w, `{"artists":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","name":"Test Artist","score":100,"type":"Group"}]}`)
		case strings.HasSuffix(r.URL.Path, "/release-group"):
			io.WriteString(w, `{"release-group-count":2,"release-groups":[{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","title":"Album One","primary-type":"Album","first-release-date":"2004-01-01"},{"id":"cccccccc-cccc-cccc-cccc-cccccccccccc","title":"Live Album","primary-type":"Album","secondary-types":["Live"]}]}`)
		case strings.HasSuffix(r.URL.Path, "/release"):
			io.WriteString(w, `{"releases":[{"id":"dddddddd-dddd-dddd-dddd-dddddddddddd","title":"Album One","country":"GB","status":"Official"}]}`)
		case strings.Contains(r.URL.Path, "/release/dddddddd"):
			io.WriteString(w, `{"media":[{"position":1,"tracks":[{"number":"1","position":1,"title":"Track 1","length":206000,"recording":{"title":"Track 1","length":206000}},{"number":"2","position":2,"title":"Track 2","length":178000}]}]}`)
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()
	original := mbBase
	mbBase = srv.URL
	defer func() { mbBase = original }()
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.lastMB = time.Time{}
	artists, err := a.findArtists(context.Background(), "Test Artist")
	if err != nil {
		t.Fatal(err)
	}
	if len(artists) != 1 || artists[0].Name != "Test Artist" {
		t.Fatalf("Artist: %#v", artists)
	}
	albums, err := a.findAlbums(context.Background(), artists[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Title != "Album One" {
		t.Fatalf("Album filtering: %#v", albums)
	}
	tracks, err := a.getAlbumTracks(context.Background(), albums[0].ID, albums[0].Title, albums[0].Date)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 || tracks[0].DurationMS != 206000 {
		t.Fatalf("Tracklist: %#v", tracks)
	}
}

func TestZipExtraction(t *testing.T) {
	dir := t.TempDir()
	zpath := filepath.Join(dir, "tools.zip")
	f, err := os.Create(zpath)
	if err != nil {
		t.Fatal(err)
	}
	zipw := zip.NewWriter(f)
	for _, name := range []string{"windows-deps/bin/ffmpeg.exe", "../ffprobe.exe"} {
		w, _ := zipw.Create(name)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1100))
	}
	_ = zipw.Close()
	_ = f.Close()
	if err := extractZip(zpath, dir, []string{"ffmpeg.exe", "ffprobe.exe"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"ffmpeg.exe", "ffprobe.exe"} {
		if !existsNonEmpty(filepath.Join(dir, v)) {
			t.Errorf("Missing %q", v)
		}
	}
}

func TestMP3Tagging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("system FFmpeg not provided in unit test")
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	mp3 := filepath.Join(dir, "01 - Test.mp3")
	c := exec.Command(path, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-q:a", "7", "-y", mp3)
	if o, err := c.CombinedOutput(); err != nil {
		t.Fatalf("fixture audio: %v %s", err, o)
	}
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.ffmpeg = path
	j := Job{Artist: "Test Artist", Album: "Test Album", Title: "Test Song", Number: "01", AlbumDate: "2004-03-02"}
	if err := a.tagTrack(context.Background(), j, mp3, dir); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-show_entries", "format_tags=title,artist,album,track,date", "-of", "json", mp3).Output()
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Format struct {
			Tags map[string]string `json:"tags"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]string{"title": "Test Song", "artist": "Test Artist", "album": "Test Album", "track": "01", "date": "2004"} {
		if got := data.Format.Tags[field]; got != want {
			t.Errorf("tag %s: %q (want %q)", field, got, want)
		}
	}
}
func Example_safePart() {
	fmt.Println(safePart("Artist: Album?")) // Output: Artist- Album-
}

func TestTrackSearchDownloadTagFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux shell fixture")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-ytdlp")
	fixture := `#!/bin/sh
output=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    ytsearch8:*)
      printf '%s\n' '{"entries":[{"id":"ABCdef12345","title":"Dreamer (Official Audio)","channel":"The Example Band - Topic","duration":201}]}'
      exit 0
      ;;
    -o) output="$2"; shift ;;
  esac
  shift
done
output=$(printf '%s' "$output" | sed 's/%(ext)s/mp3/g')
ffmpeg -hide_banner -loglevel error -f lavfi -i 'sine=frequency=440:duration=1' -q:a 7 -y "$output" || exit 1
printf '[download] 100.0%%\n'
printf 'MIXTAPE_FILE:%s\n' "$output"
`
	if err := os.WriteFile(script, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(filepath.Join(root, "data"))
	if err != nil {
		t.Fatal(err)
	}
	a.ytDLP = script
	a.ffmpeg = ffmpeg
	a.state.Settings.OutputDir = filepath.Join(root, "music")
	job := &Job{Kind: "track", Key: "track:example", Title: "Dreamer", Artist: "The Example Band", Album: "First Album", AlbumDate: "2005-04-02", Number: "1", DurationMS: 201000}
	a.addJobs([]*Job{job})
	snapshot := *a.state.Jobs[0]
	if err := a.downloadTrack(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	done, ok := a.jobCopy(snapshot.ID)
	if !ok || done.Status != "done" || done.Progress != 100 || done.Output == "" {
		t.Fatalf("Download job incomplete: %+v", done)
	}
	if !strings.HasSuffix(done.Output, filepath.Join("First Album (2005)", "01 - Dreamer.mp3")) {
		t.Fatalf("Filename not organised properly: %s", done.Output)
	}
	if info, err := os.Stat(done.Output); err != nil || info.Size() < 1000 {
		t.Fatalf("MP3 missing: %v", err)
	}
}

func TestJobLocations(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	albumFolder := filepath.Join(t.TempDir(), "Artist", "Album (2024)")
	if err := os.MkdirAll(albumFolder, 0755); err != nil {
		t.Fatal(err)
	}
	song := filepath.Join(albumFolder, "01 - Song.mp3")
	if err := os.WriteFile(song, bytes.Repeat([]byte("a"), 2048), 0644); err != nil {
		t.Fatal(err)
	}
	a.addJobs([]*Job{{Kind: "album", Key: "album:one", Title: "Album", Album: "Album"}})
	albumID := a.state.Jobs[0].ID
	a.addJobs([]*Job{
		{Kind: "track", ParentID: albumID, Key: "track:one", Title: "Song"},
		{Kind: "url", Key: "url:one", Title: "Link"},
		{Kind: "url", Key: "url:two", Title: "Unfinished"},
	})
	trackID := a.state.Jobs[1].ID
	linkID := a.state.Jobs[2].ID
	waitingID := a.state.Jobs[3].ID
	a.changeJob(trackID, func(j *Job) { j.Status = "done"; j.Output = song })
	a.changeJob(linkID, func(j *Job) { j.Status = "done"; j.Output = albumFolder })
	for _, tt := range []struct {
		name, id, want string
		file           bool
	}{
		{"track reveals MP3", trackID, song, true},
		{"album opens its folder", albumID, albumFolder, false},
		{"playlist opens its folder", linkID, albumFolder, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path, file, err := a.jobLocation(tt.id)
			if err != nil || path != tt.want || file != tt.file {
				t.Fatalf("jobLocation: path=%q file=%t err=%v", path, file, err)
			}
		})
	}
	for _, id := range []string{"unknown", waitingID} {
		if _, _, err := a.jobLocation(id); err == nil {
			t.Errorf("Expected an error for %s", id)
		}
	}
	if err := os.Remove(song); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.jobLocation(trackID); err == nil {
		t.Error("Moved/missing MP3 was accepted")
	}
	if _, _, err := a.jobLocation(albumID); err != nil {
		t.Fatal("Album folder should still be accessible:", err)
	}
}

func TestJobOpenRouteValidation(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	if rr := testPost(t, h, "/api/job/open", map[string]string{"id": "unknown"}, false); rr.Code != 403 {
		t.Errorf("Missing request header: %d", rr.Code)
	}
	if rr := testPost(t, h, "/api/job/open", map[string]string{"id": "unknown"}, true); rr.Code != 409 {
		t.Errorf("Nonexistent job: %d", rr.Code)
	}
	if rr := testPost(t, h, "/api/job/open", map[string]string{"id": strings.Repeat("a", 81)}, true); rr.Code != 400 {
		t.Errorf("Oversized job ID: %d", rr.Code)
	}
}

func TestBrowseFolderEndpoint(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	original := pickOutputFolder
	defer func() { pickOutputFolder = original }()

	if rr := testPost(t, h, "/api/settings/browse", map[string]string{"initialDir": ""}, false); rr.Code != http.StatusForbidden {
		t.Fatalf("Browser picker request without header: %d", rr.Code)
	}
	if rr := testPost(t, h, "/api/settings/browse", map[string]string{"initialDir": strings.Repeat("x", 601)}, true); rr.Code != http.StatusBadRequest {
		t.Fatalf("Long path request: %d", rr.Code)
	}
	pickOutputFolder = func(initial string) (string, bool, error) {
		if initial != a.state.Settings.OutputDir {
			t.Errorf("Expected current library as picker initial directory, got %q", initial)
		}
		return "", false, nil
	}
	rr := testPost(t, h, "/api/settings/browse", map[string]string{"initialDir": ""}, true)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"cancelled":true`) {
		t.Fatalf("Cancel should not be an error: %d %s", rr.Code, rr.Body.String())
	}
	selected := t.TempDir()
	pickOutputFolder = func(initial string) (string, bool, error) { return selected, true, nil }
	rr = testPost(t, h, "/api/settings/browse", map[string]string{"initialDir": selected}, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("Picking a folder: %d %s", rr.Code, rr.Body.String())
	}
	var data struct {
		Path      string `json:"path"`
		Cancelled bool   `json:"cancelled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil || data.Path != selected || data.Cancelled {
		t.Fatalf("Wrong selected path: %+v err=%v", data, err)
	}
	// The dialog only fills the form; saving is a separate user action.
	if a.state.Settings.OutputDir == selected {
		t.Fatal("The picker changed saved settings without Save")
	}

	pickOutputFolder = func(initial string) (string, bool, error) { return filepath.Join(selected, "missing"), true, nil }
	if rr = testPost(t, h, "/api/settings/browse", map[string]string{}, true); rr.Code != http.StatusConflict {
		t.Fatalf("Missing selected directory: %d", rr.Code)
	}
	pickOutputFolder = func(initial string) (string, bool, error) { return "", false, fmt.Errorf("dialog unavailable") }
	if rr = testPost(t, h, "/api/settings/browse", map[string]string{}, true); rr.Code != http.StatusInternalServerError {
		t.Fatalf("Picker failure should return error: %d", rr.Code)
	}
}

func TestReopenWindowEndpoint(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	// Invalid requests are rejected before any browser is launched.
	if rr := testPost(t, h, "/api/window/open", map[string]any{}, false); rr.Code != http.StatusForbidden {
		t.Fatalf("Window request without header: %d", rr.Code)
	}
}
