package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const appVersion = "1.4.1"
const listenAddress = "127.0.0.1:43875"
const browserCloseGrace = 3 * time.Second

//go:embed web/*
var webFiles embed.FS

type Candidate struct {
	Title    string `json:"title"`
	Channel  string `json:"channel"`
	URL      string `json:"url"`
	Duration int    `json:"duration"`
	Score    int    `json:"score"`
}

type Job struct {
	ID          string      `json:"id"`
	Key         string      `json:"key"`
	Kind        string      `json:"kind"` // album, track, url
	ParentID    string      `json:"parentId,omitempty"`
	Title       string      `json:"title"`
	Artist      string      `json:"artist,omitempty"`
	Album       string      `json:"album,omitempty"`
	AlbumID     string      `json:"albumId,omitempty"`
	AlbumDate   string      `json:"albumDate,omitempty"`
	Number      string      `json:"number,omitempty"`
	Disc        int         `json:"disc,omitempty"`
	DurationMS  int         `json:"durationMs,omitempty"`
	URL         string      `json:"url,omitempty"`
	ManualURL   bool        `json:"manualUrl,omitempty"`
	Status      string      `json:"status"`
	Progress    float64     `json:"progress"`
	Message     string      `json:"message,omitempty"`
	Error       string      `json:"error,omitempty"`
	ErrorDetail string      `json:"errorDetail,omitempty"`
	Output      string      `json:"output,omitempty"`
	Candidates  []Candidate `json:"candidates,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	UpdatedAt   time.Time   `json:"updatedAt"`
}

type Settings struct {
	OutputDir string `json:"outputDir"`
	KeepAwake bool   `json:"keepAwake"`
}

type StoredState struct {
	Version              int       `json:"version"`
	Jobs                 []*Job    `json:"jobs"`
	Paused               bool      `json:"paused"`
	Settings             Settings  `json:"settings"`
	LastYTDLPUpdateCheck time.Time `json:"lastYtDlpUpdateCheck,omitempty"`
	LastYTDLPUpdateOK    bool      `json:"lastYtDlpUpdateOK,omitempty"`
}

type SetupStatus struct {
	State     string  `json:"state"`
	Component string  `json:"component,omitempty"`
	Percent   float64 `json:"percent"`
	Message   string  `json:"message"`
}

type App struct {
	mu            sync.Mutex
	state         StoredState
	setup         SetupStatus
	dataDir       string
	toolsDir      string
	statePath     string
	ytDLP         string
	ffmpeg        string
	ffprobe       string
	deno          string
	activeID      string
	activeCancel  context.CancelFunc
	shutdown      func()
	mbMu          sync.Mutex
	lastMB        time.Time
	client        *http.Client
	setupRunning  bool
	updaterBusy   bool
	toolUpdate    ToolUpdateStatus
	updateCommand func(context.Context, string) (string, error)
	batchSummary  BatchSummary
	trayNotice    func(title, message string) // injectable for lifecycle tests
	sessionID     string
	stopping      chan struct{}
	stopOnce      sync.Once
	openPages     int
	pageWasOpened bool
	lastPageSeen  time.Time
	// GitHub app updates are independent from the yt-dlp helper updater.
	releaseCheckMu      sync.Mutex
	releaseUpdate       ReleaseUpdateStatus
	releaseAsset        releaseDescriptor
	releaseInstalling   bool
	releaseFetcher      func(context.Context) (releaseDescriptor, error)
	releaseDownloader   func(context.Context, *http.Client, releaseAsset, string, func(int)) error
	releaseLauncher     func(string, string, string, string, string, int) error
	currentVersion      string
	browserPages        map[string]time.Time
	departedPages       map[string]time.Time
	holdAfterBackground bool
}

func main() {
	if runtime.GOOS == "windows" {
		client := &http.Client{Timeout: 1600 * time.Millisecond}
		if r, err := client.Get("http://" + listenAddress + "/api/ping"); err == nil {
			var ping struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(io.LimitReader(r.Body, 2048)).Decode(&ping)
			_ = r.Body.Close()
			if r.StatusCode == http.StatusOK && ping.Name == "Mixtape" {
				// Ask the already-running instance to open the page in the default browser.
				req, _ := http.NewRequest(http.MethodPost, "http://"+listenAddress+"/api/window/open", strings.NewReader("{}"))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Mixtape", "1")
				if resp, err := client.Do(req); err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						return
					}
				}
				openBrowser("http://" + listenAddress) // Older server fallback.
				return
			}
		}
	}
	a, err := newApp("")
	if err != nil {
		showFatal("Mixtape could not start: " + err.Error())
		return
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		showFatal("Mixtape cannot open its local address: " + err.Error())
		return
	}
	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 7 * time.Second, IdleTimeout: 60 * time.Second}
	trayStop := startTray(a)
	shutdownComplete := make(chan struct{})
	var shutdownOnce sync.Once
	a.shutdown = func() {
		shutdownOnce.Do(func() {
			// Notify all connected browser tabs before closing the local server.
			// The page can close itself where the browser permits, otherwise it
			// shows a short closed message without touching unrelated tabs.
			a.signalShutdown()
			a.mu.Lock()
			if a.activeCancel != nil {
				a.activeCancel()
			}
			a.mu.Unlock()
			trayStop()
			go func() {
				defer close(shutdownComplete)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := srv.Shutdown(ctx); err != nil {
					_ = srv.Close()
				}
			}()
		})
	}
	go a.workerLoop()
	// Browser lifecycle is independent of the download worker, so a busy or
	// stalled audio process cannot prevent us from noticing a closed tab.
	go a.lifecycleLoop()
	if os.Getenv("MIXTAPE_TEST_NO_SETUP") != "1" {
		a.startSetup(false)
		go a.autoUpdateLoop()
		go a.releaseUpdateLoop()
	}
	log.Printf("Mixtape v%s at http://%s", appVersion, listenAddress)
	if os.Getenv("MIXTAPE_NO_BROWSER") != "1" {
		time.AfterFunc(350*time.Millisecond, func() {
			select {
			case <-a.stopping:
				return // Quit may have happened before the delayed first launch.
			default:
				openBrowser("http://" + listenAddress)
			}
		})
	}
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		// Keep the process alive until shutdown events have been flushed to
		// connected tabs; otherwise a fast process exit can drop them.
		<-shutdownComplete
	} else if err != nil {
		showFatal(err.Error())
	}
}

func newApp(root string) (*App, error) {
	if root == "" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			var err error
			base, err = os.UserConfigDir()
			if err != nil {
				return nil, err
			}
		}
		root = filepath.Join(base, "Mixtape")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	musicRoot := filepath.Join(home, "Music", "Mixtape")
	a := &App{
		dataDir: root, toolsDir: filepath.Join(root, "tools"), statePath: filepath.Join(root, "queue.json"),
		sessionID: newSessionID(), stopping: make(chan struct{}),
		state:         StoredState{Version: 1, Jobs: []*Job{}, Settings: Settings{OutputDir: musicRoot, KeepAwake: true}},
		setup:         SetupStatus{State: "checking", Message: "Checking audio tools…"},
		client:        &http.Client{Timeout: 22 * time.Second},
		toolUpdate:    ToolUpdateStatus{State: "idle"},
		updateCommand: executeYTDLPUpdate,
	}
	a.ytDLP = filepath.Join(a.toolsDir, "yt-dlp.exe")
	a.ffmpeg = filepath.Join(a.toolsDir, "ffmpeg.exe")
	a.ffprobe = filepath.Join(a.toolsDir, "ffprobe.exe")
	a.deno = filepath.Join(a.toolsDir, "deno.exe")
	a.currentVersion = installedReleaseVersion(root)
	a.releaseUpdate = ReleaseUpdateStatus{State: "checking", Current: a.currentVersion, Message: "Checking for updates…"}
	if b, err := os.ReadFile(filepath.Join(root, "last-app-update-error.txt")); err == nil {
		a.releaseUpdate.InstallError = stringNoHuge(strings.TrimSpace(string(b)))
	}
	a.releaseFetcher = fetchLatestMixtapeRelease
	a.releaseDownloader = downloadVerifiedZIP
	a.releaseLauncher = startReleaseInstaller
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "tools")
		if existsNonEmpty(filepath.Join(sibling, "yt-dlp.exe")) && existsNonEmpty(filepath.Join(sibling, "ffmpeg.exe")) && existsNonEmpty(filepath.Join(sibling, "ffprobe.exe")) && existsNonEmpty(filepath.Join(sibling, "deno.exe")) {
			a.toolsDir = sibling
			a.ytDLP = filepath.Join(sibling, "yt-dlp.exe")
			a.ffmpeg = filepath.Join(sibling, "ffmpeg.exe")
			a.ffprobe = filepath.Join(sibling, "ffprobe.exe")
			a.deno = filepath.Join(sibling, "deno.exe")
		}
	}
	if b, err := os.ReadFile(a.statePath); err == nil {
		if err := json.Unmarshal(b, &a.state); err != nil {
			return nil, fmt.Errorf("cannot read queue database: %w", err)
		}
		if a.state.Jobs == nil {
			a.state.Jobs = []*Job{}
		}
		if a.state.Settings.OutputDir == "" {
			a.state.Settings.OutputDir = musicRoot
		}
		for _, j := range a.state.Jobs {
			switch j.Status {
			case "preparing", "searching", "downloading", "tagging":
				j.Status = "queued"
				j.Message = "Resuming after restart"
				j.Progress = 0
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(a.state.Settings.OutputDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create music folder: %w", err)
	}
	return a, nil
}

// Each process gets a fresh ID, so a page from an older instance will not
// mistake a restarted backend for its original session.
func newSessionID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return newJobID()
	}
	return fmt.Sprintf("%x", id[:])
}

func (a *App) signalShutdown() {
	a.stopOnce.Do(func() { close(a.stopping) })
}

// EventSource connections are used only to deliver shutdown events. Firefox
// may keep an SSE connection alive after its tab is gone, or reconnect late.
// Neither event is allowed to change browser-presence or shutdown timestamps.
func (a *App) pageConnected() {
	a.mu.Lock()
	a.openPages++
	a.mu.Unlock()
}

func (a *App) pageDisconnected() {
	a.mu.Lock()
	if a.openPages > 0 {
		a.openPages--
	}
	a.mu.Unlock()
}

// Only a live browser tab counts as an open page. SSE connections are used
// for shutdown notifications but are not authoritative for tab presence:
// Firefox can leave one open after its tab disappears. Skipped, finished,
// failed, or review-only songs do not count as background work.
func (a *App) canExitAfterClosingPage(now time.Time) bool {
	a.mu.Lock()
	// The lease fallback must also work if the browser never sends pagehide.
	backgroundNotice := a.expireBrowserPagesLocked(now)
	canExit := a.pageWasOpened && len(a.browserPages) == 0 &&
		!a.holdAfterBackground &&
		now.Sub(a.lastPageSeen) >= browserCloseGrace &&
		!a.releaseInstalling && a.activeID == "" && !a.setupRunning && !a.updaterBusy
	a.mu.Unlock()
	if backgroundNotice {
		a.notifyTray("Downloading in background", "Mixtape will finish the selected songs.")
	}
	return canExit
}

func (a *App) notifyTray(title, message string) {
	if title == "" || message == "" {
		return
	}
	if a.trayNotice != nil {
		a.trayNotice(title, message)
		return
	}
	showTrayNotification(title, message)
}

func (a *App) exitIfPageClosedAndIdle() {
	if a.shutdown != nil && a.canExitAfterClosingPage(time.Now()) {
		a.shutdown()
	}
}

// An independent timer checks for exit even if a download worker is busy.
// The work queue is never interrupted by automatic shutdown.
func (a *App) lifecycleLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopping:
			return
		case now := <-ticker.C:
			if a.shutdown != nil && a.canExitAfterClosingPage(now) {
				a.shutdown()
			}
		}
	}
}

func (a *App) saveLocked() error {
	b, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, a.statePath); err != nil {
		// Windows can reject a replace while file is briefly indexed.
		_ = os.Remove(a.statePath)
		if err2 := os.Rename(tmp, a.statePath); err2 != nil {
			return err2
		}
	}
	return nil
}
func (a *App) changeJob(id string, fn func(*Job)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.state.Jobs {
		if j.ID == id {
			// A track that was unchecked while downloading must stay skipped,
			// even if an in-flight worker reports progress after cancellation.
			if j.Kind == "track" && j.Status == "skipped" {
				return
			}
			fn(j)
			j.UpdatedAt = time.Now()
			_ = a.saveLocked()
			return
		}
	}
}

// Toggle queued songs without removing their album/tracklist. Re-enabling a
// skipped song requeues it; already-saved files are never removed or changed.
func (a *App) setTrackEnabled(id string, enabled bool) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, job := range a.state.Jobs {
		if job.ID != id {
			continue
		}
		if job.Kind != "track" {
			return false, errors.New("only album songs can be toggled")
		}
		if job.Status == "done" {
			return false, errors.New("this song is already downloaded; its MP3 will not be deleted")
		}
		if enabled && job.Status != "skipped" || !enabled && job.Status == "skipped" {
			return false, nil
		}
		if !enabled {
			if a.activeID == id && a.activeCancel != nil {
				a.activeCancel()
			}
			job.Status = "skipped"
			job.Message = "Not selected"
		} else {
			job.Status = "queued"
			job.Message = "Waiting to download"
		}
		job.Progress = 0
		job.Error = ""
		job.ErrorDetail = ""
		job.UpdatedAt = time.Now()
		if err := a.saveLocked(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, errors.New("song not found in queue")
}
func (a *App) jobCopy(id string) (Job, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, j := range a.state.Jobs {
		if j.ID == id {
			v := *j
			return v, true
		}
	}
	return Job{}, false
}
func newJobID() string {
	return fmt.Sprintf("%d-%06d", time.Now().UnixNano(), time.Now().Nanosecond()%1000000)
}
func (a *App) addJobs(jobs []*Job) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	existing := map[string]bool{}
	for _, j := range a.state.Jobs {
		existing[j.Key] = true
	}
	count := 0
	for _, j := range jobs {
		if existing[j.Key] {
			continue
		}
		existing[j.Key] = true
		j.ID = newJobID()
		j.CreatedAt = time.Now()
		j.UpdatedAt = j.CreatedAt
		j.Status = "queued"
		a.state.Jobs = append(a.state.Jobs, j)
		count++
		time.Sleep(time.Nanosecond)
	}
	if count > 0 {
		_ = a.saveLocked()
	}
	return count
}

// pickOutputFolder is replaceable in tests; production uses the OS-native picker.
var pickOutputFolder = chooseFolder

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	webroot, _ := fs.Sub(webFiles, "web")
	fileServer := http.FileServer(http.FS(webroot))
	mux.Handle("/", fileServer)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, http.StatusOK, map[string]any{"name": "Mixtape", "version": a.currentVersion})
	})
	// Server-sent shutdown notification. A live tab receives this immediately
	// when Quit is selected in the system tray or Settings. No browser-specific
	// processes need to be launched, tracked, or terminated.
	mux.HandleFunc("/api/lifecycle", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		a.pageConnected()
		defer a.pageDisconnected()
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()
		select {
		case <-a.stopping:
			fmt.Fprint(w, "event: shutdown\ndata: closed\n\n")
			flusher.Flush()
		case <-r.Context().Done():
		}
	})
	// A polling page refreshes its own presence lease. Browser tabs are
	// tracked separately from SSE so a stale Firefox connection cannot pin
	// the backend in the tray.
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		a.pagePresent(r.Header.Get("X-Mixtape-Page"))
		a.mu.Lock()
		defer a.mu.Unlock()
		jsonOut(w, 200, map[string]any{"jobs": a.state.Jobs, "paused": a.state.Paused, "settings": a.state.Settings, "setup": a.setup, "toolUpdate": a.toolUpdate, "version": a.currentVersion, "sessionID": a.sessionID, "queueStatus": a.queueStatusLocked(), "appUpdate": a.releaseUpdate})
	})
	// sendBeacon on pagehide works in Firefox without needing a fetch to
	// complete while the browser tab is closing. Requiring this backend's
	// session ID prevents unrelated pages from shutting down tabs.
	mux.HandleFunc("/api/page/leave", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "JSON required", http.StatusUnsupportedMediaType)
			return
		}
		var data struct {
			PageID    string `json:"pageID"`
			SessionID string `json:"sessionID"`
		}
		if !decode(w, r, &data) {
			return
		}
		if !a.pageLeft(data.PageID, data.SessionID) {
			http.Error(w, "Unknown page", http.StatusConflict)
			return
		}
		// A second beforeunload/pagehide delivery is an idempotent success.
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/artists", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if len(q) < 2 || len(q) > 120 {
			http.Error(w, "Enter at least two characters", 400)
			return
		}
		ar, err := a.findArtists(r.Context(), q)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		jsonOut(w, 200, ar)
	})
	mux.HandleFunc("/api/albums", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("artistId")
		if !validUUID(id) {
			http.Error(w, "Invalid artist", 400)
			return
		}
		al, err := a.findAlbums(r.Context(), id)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		jsonOut(w, 200, al)
	})
	mux.HandleFunc("/api/queue/albums", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			Artist string `json:"artist"`
			Albums []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
				Date  string `json:"date"`
			} `json:"albums"`
		}
		if !decode(w, r, &p) {
			return
		}
		if len(p.Artist) == 0 || len(p.Artist) > 180 || len(p.Albums) > 70 || len(p.Albums) == 0 {
			http.Error(w, "Invalid artist or albums", 400)
			return
		}
		jobs := []*Job{}
		for _, v := range p.Albums {
			if !validUUID(v.ID) || len(v.Title) == 0 || len(v.Title) > 220 || !validAlbumDate(v.Date) {
				http.Error(w, "Invalid album", 400)
				return
			}
			jobs = append(jobs, &Job{Kind: "album", Title: v.Title, Artist: p.Artist, Album: v.Title, AlbumID: v.ID, AlbumDate: v.Date, Key: "album:" + v.ID, Message: "Waiting to get tracklist"})
		}
		n := a.addJobs(jobs)
		jsonOut(w, 200, map[string]int{"added": n})
	})
	mux.HandleFunc("/api/queue/urls", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			Text string `json:"text"`
		}
		if !decode(w, r, &p) {
			return
		}
		lines := strings.Split(strings.ReplaceAll(p.Text, "\r", ""), "\n")
		if len(lines) > 120 {
			http.Error(w, "Maximum 120 links at a time", 400)
			return
		}
		jobs := []*Job{}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			v, err := validateVideoURL(line)
			if err != nil {
				http.Error(w, "Invalid YouTube link: "+line, 400)
				return
			}
			jobs = append(jobs, &Job{Kind: "url", Key: "url:" + v, Title: "YouTube link", URL: v, Message: "Waiting to download"})
		}
		if len(jobs) == 0 {
			http.Error(w, "Paste at least one YouTube link", 400)
			return
		}
		n := a.addJobs(jobs)
		jsonOut(w, 200, map[string]int{"added": n})
	})
	mux.HandleFunc("/api/job/review", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		}
		if !decode(w, r, &p) {
			return
		}
		u, err := validateVideoURL(p.URL)
		if err != nil {
			http.Error(w, "Invalid YouTube URL", 400)
			return
		}
		found := false
		a.changeJob(p.ID, func(j *Job) {
			if j.Status == "review" && j.Kind == "track" {
				j.URL = u
				j.ManualURL = true
				j.Status = "queued"
				j.Error = ""
				j.ErrorDetail = ""
				j.Message = "Selected match is queued"
				found = true
			}
		})
		if !found {
			http.Error(w, "Track not found or not under review", 404)
			return
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/job/retry", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &p) {
			return
		}
		a.changeJob(p.ID, func(j *Job) {
			if j.Status == "failed" || j.Status == "review" {
				j.Status = "queued"
				j.Error = ""
				j.ErrorDetail = ""
				j.Progress = 0
				j.Message = "Waiting to retry"
			}
		})
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/job/select", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		}
		if !decode(w, r, &p) {
			return
		}
		if len(p.ID) == 0 || len(p.ID) > 80 || p.Enabled == nil {
			http.Error(w, "Invalid song selection", http.StatusBadRequest)
			return
		}
		changed, err := a.setTrackEnabled(p.ID, *p.Enabled)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonOut(w, http.StatusOK, map[string]bool{"changed": changed})
	})
	mux.HandleFunc("/api/queue/retry-failed", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		a.mu.Lock()
		count := retryFailedLocked(&a.state, time.Now())
		if count > 0 {
			_ = a.saveLocked()
		}
		a.mu.Unlock()
		jsonOut(w, http.StatusOK, map[string]int{"retried": count})
	})
	mux.HandleFunc("/api/job/remove", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &p) {
			return
		}
		a.mu.Lock()
		if a.activeID == p.ID && a.activeCancel != nil {
			a.activeCancel()
		}
		removed := false
		selectedAlbum := false
		for _, j := range a.state.Jobs {
			if j.ID == p.ID && j.Kind == "album" {
				selectedAlbum = true
			}
		}
		keep := a.state.Jobs[:0]
		for _, j := range a.state.Jobs {
			if j.ID == p.ID || (selectedAlbum && j.ParentID == p.ID) {
				if a.activeID == j.ID && a.activeCancel != nil {
					a.activeCancel()
				}
				removed = true
				continue
			}
			keep = append(keep, j)
		}
		a.state.Jobs = keep
		if removed {
			_ = a.saveLocked()
		}
		a.mu.Unlock()
		jsonOut(w, 200, map[string]bool{"removed": removed})
	})
	mux.HandleFunc("/api/queue/clear", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		a.mu.Lock()
		// Keep all tracks of a partly-finished album, so clearing history
		// never makes the remaining songs disappear from the interface.
		keepAlbum := map[string]bool{}
		for _, j := range a.state.Jobs {
			if j.Kind == "album" && j.Status != "done" && j.Status != "failed" {
				keepAlbum[j.ID] = true
			}
		}
		for _, j := range a.state.Jobs {
			if j.Kind == "track" && j.Status != "done" && j.Status != "failed" {
				keepAlbum[j.ParentID] = true
			}
		}
		keep := a.state.Jobs[:0]
		for _, j := range a.state.Jobs {
			if (j.Kind == "album" && keepAlbum[j.ID]) || (j.Kind == "track" && keepAlbum[j.ParentID]) || (j.Kind == "url" && j.Status != "done" && j.Status != "failed") {
				keep = append(keep, j)
			}
		}
		a.state.Jobs = keep
		_ = a.saveLocked()
		a.mu.Unlock()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/queue/pause", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			Paused bool `json:"paused"`
		}
		if !decode(w, r, &p) {
			return
		}
		a.mu.Lock()
		a.state.Paused = p.Paused
		_ = a.saveLocked()
		a.mu.Unlock()
		jsonOut(w, 200, map[string]bool{"paused": p.Paused})
	})
	mux.HandleFunc("/api/settings/browse", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			InitialDir string `json:"initialDir"`
		}
		if !decode(w, r, &p) {
			return
		}
		if len(p.InitialDir) > 600 {
			http.Error(w, "Folder path too long", http.StatusBadRequest)
			return
		}
		if p.InitialDir == "" {
			a.mu.Lock()
			p.InitialDir = a.state.Settings.OutputDir
			a.mu.Unlock()
		}
		selected, ok, err := pickOutputFolder(p.InitialDir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ok {
			info, err := os.Stat(selected)
			if err != nil || !info.IsDir() || !filepath.IsAbs(selected) {
				http.Error(w, "Selected folder is unavailable", http.StatusConflict)
				return
			}
		}
		jsonOut(w, 200, map[string]any{"path": selected, "cancelled": !ok})
	})
	mux.HandleFunc("/api/window/open", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		go openBrowser("http://" + listenAddress)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p Settings
		if !decode(w, r, &p) {
			return
		}
		clean := filepath.Clean(strings.TrimSpace(p.OutputDir))
		if len(clean) < 3 || len(clean) > 600 || !filepath.IsAbs(clean) {
			http.Error(w, "Use an absolute folder path", 400)
			return
		}
		if err := os.MkdirAll(clean, 0755); err != nil {
			http.Error(w, "Could not create folder: "+err.Error(), 400)
			return
		}
		a.mu.Lock()
		a.state.Settings = Settings{OutputDir: clean, KeepAwake: p.KeepAwake}
		_ = a.saveLocked()
		a.mu.Unlock()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/job/open", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		var p struct {
			ID string `json:"id"`
		}
		if !decode(w, r, &p) {
			return
		}
		if len(p.ID) == 0 || len(p.ID) > 80 {
			http.Error(w, "Invalid job", http.StatusBadRequest)
			return
		}
		location, selectFile, err := a.jobLocation(p.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if err := openLocation(location, selectFile); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/library/open", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		a.mu.Lock()
		dir := a.state.Settings.OutputDir
		a.mu.Unlock()
		if err := openFolder(dir); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/tools/retry", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		a.mu.Lock()
		working := a.activeID != "" || a.updaterBusy || a.releaseInstalling
		a.mu.Unlock()
		if working {
			http.Error(w, "Wait until the current song has finished before refreshing the downloader", 409)
			return
		}
		a.startSetup(true)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("/api/tools/update", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		if err := a.requestYTDLPUpdate(true); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
	})
	// Updates use GitHub's public latest-release API. Checking never modifies
	// the program; installing is a separate explicit action while the queue is idle.
	mux.HandleFunc("/api/app-update/check", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		go func() { _ = a.checkReleaseUpdates(context.Background(), true) }()
		jsonOut(w, http.StatusAccepted, map[string]bool{"started": true})
	})
	mux.HandleFunc("/api/app-update/apply", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		if err := a.beginReleaseUpdate(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		jsonOut(w, http.StatusAccepted, map[string]bool{"started": true})
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		if !mustPost(w, r) {
			return
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
		go func() {
			time.Sleep(150 * time.Millisecond)
			if a.shutdown != nil {
				a.shutdown()
			}
		}()
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listenAddress && os.Getenv("MIXTAPE_ALLOW_TEST_HOST") != "1" {
			http.Error(w, "This app is only available on its local address", 403)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		// Bound to 127.0.0.1 only. Keep web pages elsewhere from invoking local POST APIs.
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method == "POST" {
			origin := r.Header.Get("Origin")
			if origin != "" && origin != "http://"+r.Host {
				http.Error(w, "Origin not allowed", 403)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

// jobLocation returns a verified on-disk destination, not a caller-supplied path.
// A track opens selected in Explorer; an album opens the folder containing its
// completed tracks. Old downloads remain accessible after changing settings.
func (a *App) jobLocation(id string) (location string, selectFile bool, err error) {
	a.mu.Lock()
	var job *Job
	var children []string
	for _, current := range a.state.Jobs {
		if current.ID == id {
			copyOfJob := *current
			job = &copyOfJob
		}
	}
	if job != nil && job.Kind == "album" {
		for _, current := range a.state.Jobs {
			if current.ParentID == id && current.Status == "done" && current.Output != "" {
				children = append(children, current.Output)
			}
		}
	}
	a.mu.Unlock()
	if job == nil {
		return "", false, fmt.Errorf("download not found in the queue")
	}
	if job.Kind == "album" {
		for _, output := range children {
			path := filepath.Dir(output)
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				return path, false, nil
			}
		}
		return "", false, fmt.Errorf("no completed songs from this album are available on disk")
	}
	if job.Status != "done" || job.Output == "" {
		return "", false, fmt.Errorf("this item hasn't finished downloading yet")
	}
	path := filepath.Clean(job.Output)
	info, statErr := os.Stat(path)
	if statErr != nil {
		return "", false, fmt.Errorf("downloaded file or folder is missing: %w", statErr)
	}
	return path, !info.IsDir(), nil
}

func mustPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "POST" {
		http.Error(w, "POST required", 405)
		return false
	}
	if r.Header.Get("X-Mixtape") != "1" {
		http.Error(w, "Request header required", 403)
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "JSON required", 415)
		return false
	}
	return true
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 100*1024)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "Bad JSON input", 400)
		return false
	}
	return true
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func validAlbumDate(s string) bool {
	return s == "" || regexp.MustCompile(`^[0-9]{4}(-[0-9]{2}(-[0-9]{2})?)?$`).MatchString(s)
}

func validUUID(s string) bool {
	return regexp.MustCompile(`(?i)^[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}$`).MatchString(s)
}
func validateVideoURL(raw string) (string, error) {
	if len(raw) > 2000 {
		return "", errors.New("URL too long")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", errors.New("only HTTPS YouTube links are supported")
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "youtu.be":
		if len(strings.Trim(u.Path, "/")) < 5 {
			return "", errors.New("missing video ID")
		}
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com":
		if !(u.Path == "/watch" || u.Path == "/playlist" || strings.HasPrefix(u.Path, "/shorts/") || strings.HasPrefix(u.Path, "/live/") || strings.HasPrefix(u.Path, "/embed/")) {
			return "", errors.New("link must be a video or playlist")
		}
	default:
		return "", errors.New("not a YouTube address")
	}
	u.Fragment = ""
	return u.String(), nil
}
func safePart(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*', '\x00', '\n', '\r', '\t':
			return '-'
		}
		if r < 32 {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(strings.TrimSpace(s), ". ")
	if len([]rune(s)) > 95 {
		s = string([]rune(s)[:95])
	}
	if s == "" || s == "." || s == ".." {
		s = "Unknown"
	}
	p := strings.ToUpper(strings.Split(s, ".")[0])
	for _, reserved := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"} {
		if p == reserved {
			s = "_" + s
			break
		}
	}
	return s
}
func albumDir(base string, j Job) string {
	album := safePart(j.Album)
	if len(j.AlbumDate) >= 4 {
		album += " (" + j.AlbumDate[:4] + ")"
	}
	return filepath.Join(base, safePart(j.Artist), album)
}
func trackFileNumber(s string) string {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < 100 {
		return fmt.Sprintf("%02d", n)
	}
	parts := strings.Split(s, "-")
	if len(parts) == 2 {
		if disc, e1 := strconv.Atoi(parts[0]); e1 == nil && disc > 0 && disc < 20 {
			if track, e2 := strconv.Atoi(parts[1]); e2 == nil && track > 0 && track < 100 {
				return fmt.Sprintf("%d-%02d", disc, track)
			}
		}
	}
	return s
}

func yearFromDate(s string) string {
	if len(s) >= 4 {
		if _, err := strconv.Atoi(s[:4]); err == nil {
			return s[:4]
		}
	}
	return ""
}
func prettyJob(j Job) string {
	if j.Kind == "track" {
		return j.Artist + " - " + j.Title
	}
	if j.Kind == "album" {
		return j.Artist + " - " + j.Album
	}
	return j.URL
}
func stringNoHuge(s string) string {
	if len(s) > 360 {
		return s[:360] + "…"
	}
	return s
}
func (a *App) updateSetup(state, component, message string, p float64) {
	a.mu.Lock()
	a.setup = SetupStatus{State: state, Component: component, Message: message, Percent: p}
	a.mu.Unlock()
}
func (a *App) nextJob() (Job, context.Context, context.CancelFunc, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Paused || a.setup.State != "ready" || a.updaterBusy || a.releaseInstalling {
		return Job{}, nil, nil, false
	}
	for _, j := range a.state.Jobs {
		if j.Status == "queued" {
			// Install cancellation before exposing the claimed job. Otherwise
			// removing a job immediately after claiming it can miss the cancel.
			ctx, cancel := context.WithCancel(context.Background())
			a.activeID = j.ID
			a.activeCancel = cancel
			j.Status = "preparing"
			j.Message = "Getting ready…"
			j.Progress = 0
			j.UpdatedAt = time.Now()
			_ = a.saveLocked()
			copy := *j
			return copy, ctx, cancel, true
		}
	}
	return Job{}, nil, nil, false
}
func (a *App) workerLoop() {
	runtime.LockOSThread() // Windows sleep-prevention belongs to one OS thread.
	defer runtime.UnlockOSThread()
	for {
		select {
		case <-a.stopping:
			return
		default:
		}
		j, ctx, cancel, ok := a.nextJob()
		if !ok {
			a.exitIfPageClosedAndIdle()
			select {
			case <-a.stopping:
				return
			case <-time.After(850 * time.Millisecond):
			}
			continue
		}
		a.mu.Lock()
		keepAwake := a.state.Settings.KeepAwake
		a.mu.Unlock()
		if keepAwake {
			setKeepAwake(true)
		}
		err := a.handleJob(ctx, j)
		if keepAwake {
			setKeepAwake(false)
		}
		cancel()
		a.mu.Lock()
		a.activeCancel = nil
		a.activeID = ""
		a.mu.Unlock()
		if err != nil && !errors.Is(err, context.Canceled) {
			summary, detail := jobFailure(err)
			a.changeJob(j.ID, func(p *Job) {
				p.Status = "failed"
				p.Error = summary
				p.ErrorDetail = detail
				p.Message = "Download failed"
			})
		}
		a.finishBatchAndNotify(j.ID)
	}
}
