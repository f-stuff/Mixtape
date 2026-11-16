package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetadataRetriesIntermittentSearchFailures(t *testing.T) {
	var artistCalls, albumCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("User-Agent"), "Mixtape/") {
			t.Error("Metadata requests must identify the application")
		}
		switch r.URL.Path {
		case "/artist":
			n := artistCalls.Add(1)
			if n <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `{"artists":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","name":"Example","score":100}]}`)
		case "/release-group":
			n := albumCalls.Add(1)
			if n <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			io.WriteString(w, `{"release-group-count":1,"release-groups":[{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","title":"Example Album","primary-type":"Album"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldBase := mbBase
	mbBase = srv.URL
	defer func() { mbBase = oldBase }()
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.client = srv.Client()
	artists, err := a.findArtists(context.Background(), "Example")
	if err != nil || len(artists) != 1 || artistCalls.Load() != 3 {
		t.Fatalf("artist search did not retry transient failures: %v, %+v, requests=%d", err, artists, artistCalls.Load())
	}
	albums, err := a.findAlbums(context.Background(), "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil || len(albums) != 1 || albumCalls.Load() != 3 {
		t.Fatalf("album search did not retry transient failures: %v, %+v, requests=%d", err, albums, albumCalls.Load())
	}
}

func TestMetadataDoesNotRetryPermanentFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "invalid", http.StatusBadRequest)
	}))
	defer srv.Close()
	oldBase := mbBase
	mbBase = srv.URL
	defer func() { mbBase = oldBase }()
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var payload any
	if err := a.mbGET(context.Background(), "/artist", &payload); err == nil || calls.Load() != 1 {
		t.Fatalf("permanent error should not retry: %v, calls=%d", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.mbGET(ctx, "/artist", &payload); err != context.Canceled || calls.Load() != 1 {
		t.Fatalf("cancelled search should stop immediately: %v, calls=%d", err, calls.Load())
	}
}

func TestSongSelectionPersistsAndResumes(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Kind: "album", Key: "album:example", Title: "Example", Album: "Example"}})
	parentID := a.state.Jobs[0].ID
	a.addJobs([]*Job{{Kind: "track", Key: "track:a", ParentID: parentID, Title: "One"}, {Kind: "track", Key: "track:b", ParentID: parentID, Title: "Two"}})
	first := a.state.Jobs[1].ID
	second := a.state.Jobs[2].ID
	selected := false
	resp := testPost(t, a.routes(), "/api/job/select", map[string]any{"id": first, "enabled": selected}, true)
	if resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	j, _ := a.jobCopy(first)
	if j.Status != "skipped" {
		t.Fatalf("Not skipped: %+v", j)
	}
	reopened, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	j, _ = reopened.jobCopy(first)
	if j.Status != "skipped" {
		t.Fatalf("Skipped selection not persisted: %+v", j)
	}
	reopened.setup.State = "ready"
	job, _, cancel, ok := reopened.nextJob()
	if !ok || job.ID != a.state.Jobs[0].ID {
		t.Fatalf("Album should get processed first: %+v", job)
	}
	cancel()
	reopened.changeJob(job.ID, func(j *Job) { j.Status = "done" })
	reopened.mu.Lock()
	reopened.activeID = ""
	reopened.activeCancel = nil
	reopened.mu.Unlock()
	job, _, cancel, ok = reopened.nextJob()
	if !ok || job.ID != second {
		t.Fatalf("Skipped track was claimed: %+v", job)
	}
	cancel()
	reopened.changeJob(job.ID, func(j *Job) { j.Status = "done" })
	reopened.mu.Lock()
	reopened.activeID = ""
	reopened.activeCancel = nil
	reopened.mu.Unlock()
	if _, _, _, ok := reopened.nextJob(); ok {
		t.Fatal("A skipped track should not be processed")
	}
	selected = true
	resp = testPost(t, reopened.routes(), "/api/job/select", map[string]any{"id": first, "enabled": selected}, true)
	if resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	job, _, cancel, ok = reopened.nextJob()
	if !ok || job.ID != first {
		t.Fatalf("Re-enabled track did not queue: %+v", job)
	}
	cancel()
}

func TestUncheckingInFlightSongCancelsAndCannotBeOverwritten(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Kind: "track", Key: "track:active", Title: "Active"}})
	job, ctx, cancel, ok := a.nextJob()
	if !ok {
		t.Fatal("Job not claimed")
	}
	defer cancel()
	resp := testPost(t, a.routes(), "/api/job/select", map[string]any{"id": job.ID, "enabled": false}, true)
	if resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Unchecked active song was not cancelled")
	}
	a.changeJob(job.ID, func(j *Job) { j.Status = "done"; j.Message = "Incorrectly finished" })
	j, _ := a.jobCopy(job.ID)
	if j.Status != "skipped" {
		t.Fatalf("Worker overrode the skipped state: %+v", j)
	}
	// Invalid requests should not toggle album jobs or saved MP3s.
	bad := testPost(t, a.routes(), "/api/job/select", map[string]any{"id": job.ID}, true)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("Missing enabled accepted: %d", bad.Code)
	}
	resp = testPost(t, a.routes(), "/api/job/select", map[string]any{"id": job.ID, "enabled": true}, true)
	if resp.Code != http.StatusOK {
		t.Fatal(resp.Body.String())
	}
	a.changeJob(job.ID, func(j *Job) { j.Status = "done"; j.Output = "C:\\Music\\Song.mp3" })
	bad = testPost(t, a.routes(), "/api/job/select", map[string]any{"id": job.ID, "enabled": false}, true)
	if bad.Code != http.StatusConflict {
		t.Fatalf("Downloaded song toggled: %d", bad.Code)
	}
}

// The reporter's case: an album has 12 tracks, six are downloaded and six
// are unchecked. No selected work remains, even though the album and all its
// skipped track entries remain in queue.json. A stale Firefox SSE socket must
// not keep the tray app running.
func TestAutoQuitSixDoneSixSkippedEvenWithStaleSSE(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Kind: "album", Key: "album:test", Title: "Test", Album: "Test"}})
	parentID := a.state.Jobs[0].ID
	a.changeJob(parentID, func(j *Job) { j.Status = "done" })
	jobs := []*Job{}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &Job{Kind: "track", Key: fmt.Sprintf("album:track:%d", i), ParentID: parentID, Title: fmt.Sprintf("Track %d", i+1)})
	}
	if count := a.addJobs(jobs); count != 12 {
		t.Fatalf("added %d tracks", count)
	}
	for i, j := range jobs {
		if i < 6 {
			a.changeJob(j.ID, func(p *Job) { p.Status = "done"; p.Message = "Ready in library" })
		} else if changed, err := a.setTrackEnabled(j.ID, false); err != nil || !changed {
			t.Fatalf("Cannot uncheck track %d: %v", i, err)
		}
	}
	a.pageConnected() // Firefox can leave this connection around after closing.
	pageID := "six-done-six-skipped-1234"
	a.pagePresent(pageID)
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Open browser tab must block auto-exit")
	}
	if !a.pageLeft(pageID, a.sessionID) {
		t.Fatal("Browser close was not registered")
	}
	if a.canExitAfterClosingPage(time.Now()) {
		t.Fatal("Closing grace period missing")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Six done and six skipped must count as finished, even with a stale SSE connection")
	}
	quitCalled := false
	a.shutdown = func() { quitCalled = true }
	a.mu.Lock()
	a.lastPageSeen = time.Now().Add(-browserCloseGrace - time.Second)
	a.mu.Unlock()
	a.exitIfPageClosedAndIdle()
	if !quitCalled {
		t.Fatal("Idle application did not exit")
	}
	reopened, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	var done, skipped, queued int
	for _, j := range reopened.state.Jobs {
		if j.Kind == "track" {
			switch j.Status {
			case "done":
				done++
			case "skipped":
				skipped++
			case "queued":
				queued++
			}
		}
	}
	if done != 6 || skipped != 6 || queued != 0 {
		t.Fatalf("Queue changed on exit: done=%d skipped=%d queued=%d", done, skipped, queued)
	}
}

// Closing while work is still queued must finish in the tray and notify.
// Once complete, opening and closing the app again should release the tray.
func TestAutoQuitAfterBackgroundBatchWasReopened(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	jobs := []*Job{}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &Job{Kind: "track", Key: fmt.Sprintf("batch:%d", i), Title: fmt.Sprintf("Track %d", i)})
	}
	a.addJobs(jobs)
	for i := 6; i < 12; i++ {
		if _, err := a.setTrackEnabled(jobs[i].ID, false); err != nil {
			t.Fatal(err)
		}
	}
	a.pagePresent("first-browser-tab-1234")
	if !a.pageLeft("first-browser-tab-1234", a.sessionID) {
		t.Fatal("First close failed")
	}
	future := time.Now().Add(browserCloseGrace + time.Second)
	if a.canExitAfterClosingPage(future) {
		t.Fatal("Selected queued downloads must continue in the tray")
	}
	for i := 0; i < 6; i++ {
		a.changeJob(jobs[i].ID, func(j *Job) { j.Status = "done" })
	}
	a.mu.Lock()
	a.batchSummary = BatchSummary{Songs: 5} // the final result will make six
	a.mu.Unlock()
	if message := a.recordBatchResult(jobs[5].ID); message != "6 songs saved" {
		t.Fatalf("Notification expected after finishing selected tracks, got %q", message)
	}
	if a.canExitAfterClosingPage(future) {
		t.Fatal("Hold tray after background completion until user can reopen it")
	}
	a.pagePresent("second-browser-tab-1234")
	if a.canExitAfterClosingPage(future) {
		t.Fatal("Cannot exit with reopened page visible")
	}
	if !a.pageLeft("second-browser-tab-1234", a.sessionID) {
		t.Fatal("Second close failed")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Reopening and closing after batch completion must end Mixtape")
	}
}

func TestAutoQuitMultipleTabsPausedQueueAndUpdate(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Kind: "url", Key: "test:1", URL: "https://youtu.be/abcdefghijk"}})
	a.state.Paused = true
	a.pagePresent("first-browser-tab-1234")
	a.pagePresent("second-browser-tab-1234")
	if !a.pageLeft("first-browser-tab-1234", a.sessionID) {
		t.Fatal("First tab close failed")
	}
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Other browser tab still open")
	}
	if !a.pageLeft("second-browser-tab-1234", a.sessionID) {
		t.Fatal("Second tab close failed")
	}
	future := time.Now().Add(browserCloseGrace + time.Second)
	if !a.canExitAfterClosingPage(future) {
		t.Fatal("Paused jobs are not actually downloading")
	}
	a.mu.Lock()
	a.updaterBusy = true
	a.mu.Unlock()
	if a.canExitAfterClosingPage(future) {
		t.Fatal("Do not interrupt active updater")
	}
	a.mu.Lock()
	a.updaterBusy = false
	a.setupRunning = true
	a.mu.Unlock()
	if a.canExitAfterClosingPage(future) {
		t.Fatal("Do not interrupt first-time tool setup")
	}
	a.mu.Lock()
	a.setupRunning = false
	a.mu.Unlock()
	if !a.canExitAfterClosingPage(future) {
		t.Fatal("Exit after maintenance finishes")
	}
}

func TestBrowserPresenceLeaseExpiresAndIgnoresLatePoll(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "browser-tab-close-race-1234"
	a.pagePresent(id)
	if a.pageLeft(id, "wrong-session") {
		t.Fatal("Unrelated instance closed our page")
	}
	if len(a.browserPages) != 1 {
		t.Fatal("Bad session changed active pages")
	}
	if !a.pageLeft(id, a.sessionID) {
		t.Fatal("Expected tab removal")
	}
	a.pagePresent(id) // A poll that began before pagehide finishes late.
	if len(a.browserPages) != 0 {
		t.Fatal("Late poll revived a closed Firefox tab")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Late poll prevented idle shutdown")
	}
	other := "browser-tab-crashed-1234"
	a.pagePresent(other)
	a.mu.Lock()
	a.browserPages[other] = time.Now().Add(-pageLeaseTimeout - time.Second)
	a.mu.Unlock()
	firstCheck := time.Now()
	if a.canExitAfterClosingPage(firstCheck) {
		t.Fatal("Expired lease still needs the closing grace period")
	}
	if !a.canExitAfterClosingPage(firstCheck.Add(browserCloseGrace + time.Second)) {
		t.Fatal("Stale page lease never expired")
	}
}

// Exercise the actual HTTP handlers used by fetch and sendBeacon rather than
// only calling the internal lifecycle helpers.
func TestBrowserPageCloseBeaconOverHTTP(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	client := server.Client()
	id := "tab-http-beacon-test-1234"
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = listenAddress
	req.Header.Set("X-Mixtape-Page", id)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("Presence GET status %d", res.StatusCode)
	}
	if len(a.browserPages) != 1 {
		t.Fatal("Page heartbeat was not registered")
	}
	body, _ := json.Marshal(map[string]string{"pageID": id, "sessionID": a.sessionID})
	req, err = http.NewRequest(http.MethodPost, server.URL+"/api/page/leave", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = listenAddress
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+listenAddress)
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("Beacon status %d", res.StatusCode)
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Real browser heartbeat / pagehide beacon did not allow idle exit")
	}
	// A malicious cross-origin page cannot forge a same-origin JSON beacon.
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/api/page/leave", bytes.NewReader(body))
	req.Host = listenAddress
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://other.example")
	res, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("Cross-site close was not rejected: %d", res.StatusCode)
	}
}

func TestLifecyclePageConnections(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.routes())
	defer server.Close()
	// Host restriction is intentional; use a custom transport that connects
	// to the test server but preserves the loopback Host header.
	client := server.Client()
	req, err := http.NewRequest("GET", server.URL+"/api/lifecycle", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = listenAddress
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 50)
	_, _ = res.Body.Read(buf)
	a.mu.Lock()
	count := a.openPages
	a.mu.Unlock()
	if count != 1 {
		t.Fatalf("Lifecycle stream did not register browser: %d", count)
	}
	cancel()
	_ = res.Body.Close()
	deadline := time.After(2 * time.Second)
	for {
		a.mu.Lock()
		count = a.openPages
		a.mu.Unlock()
		if count == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Did not unregister disconnected tab")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Keep the Go JSON representation explicit: no client can turn an album job
// into a track by omitting a field or sending a string for the enabled flag.
func TestSongSelectionInputTypes(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.addJobs([]*Job{{Kind: "album", Key: "album", Title: "Test"}})
	id := a.state.Jobs[0].ID
	r := testPost(t, a.routes(), "/api/job/select", map[string]any{"id": id, "enabled": true}, true)
	if r.Code != http.StatusConflict {
		t.Fatalf("Album checkbox should be rejected: %d", r.Code)
	}
	malformed := testPost(t, a.routes(), "/api/job/select", map[string]any{"id": id, "enabled": "sometimes"}, true)
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("Malformed bool should be rejected: %d", malformed.Code)
	}
	// State JSON still serializes to the original queue schema.
	if _, err := json.Marshal(a.state); err != nil {
		t.Fatal(err)
	}
}
