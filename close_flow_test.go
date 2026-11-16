package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedTrayNotice struct {
	title, message string
}

// The main real-world case: 12 tracks, six unchecked, six already downloaded.
// A late/disconnected SSE socket cannot reset the tab-close timestamp or
// prevent the independent lifecycle monitor from exiting the idle backend.
func TestCloseFlowSixDoneSixSkippedWithLateSSE(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	parent := &Job{Kind: "album", Key: "closeflow:album", Title: "Twelve", Album: "Twelve", Status: "done"}
	a.addJobs([]*Job{parent})
	a.changeJob(parent.ID, func(j *Job) { j.Status = "done" })
	for i := 0; i < 12; i++ {
		job := &Job{Kind: "track", Key: fmt.Sprintf("closeflow:%d", i), Title: fmt.Sprintf("Track %d", i), ParentID: parent.ID}
		a.addJobs([]*Job{job})
		if i < 6 {
			a.changeJob(job.ID, func(j *Job) { j.Status = "done" })
		} else if _, err := a.setTrackEnabled(job.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.queueStatus(); got != "Finished" {
		t.Fatalf("queue: %q", got)
	}
	id := "finished-browser-close-1234"
	a.pagePresent(id)
	a.pageConnected()
	if !a.pageLeft(id, a.sessionID) {
		t.Fatal("close failed")
	}
	a.mu.Lock()
	closedAt := a.lastPageSeen
	a.mu.Unlock()
	a.pageConnected() // A late Firefox EventSource reconnect.
	a.pageDisconnected()
	a.mu.Lock()
	unchanged := a.lastPageSeen.Equal(closedAt)
	a.mu.Unlock()
	if !unchanged {
		t.Fatal("SSE connection changed the tab-close timestamp")
	}
	if !a.canExitAfterClosingPage(closedAt.Add(browserCloseGrace + time.Millisecond)) {
		t.Fatal("6 downloaded + 6 skipped should exit despite live SSE")
	}
}

func TestCloseFlowBackgroundNotificationsAndReopen(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	var notices []capturedTrayNotice
	a.trayNotice = func(title, message string) {
		notices = append(notices, capturedTrayNotice{title, message})
	}
	jobs := []*Job{}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &Job{Kind: "track", Key: fmt.Sprintf("batch:notification:%d", i), Title: fmt.Sprintf("Track %d", i)})
	}
	a.addJobs(jobs)
	for i := 6; i < 12; i++ {
		if _, err := a.setTrackEnabled(jobs[i].ID, false); err != nil {
			t.Fatal(err)
		}
	}
	a.pagePresent("first-window-open-1234")
	if !a.pageLeft("first-window-open-1234", a.sessionID) {
		t.Fatal("close failed")
	}
	if len(notices) != 1 || notices[0].title != "Downloading in background" {
		t.Fatalf("expected one background notification, got %+v", notices)
	}
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("selected pending downloads must keep running")
	}
	for i := 0; i < 6; i++ {
		a.changeJob(jobs[i].ID, func(j *Job) { j.Status = "done" })
		a.finishBatchAndNotify(jobs[i].ID)
	}
	if len(notices) != 2 || notices[1].title != "Done" || notices[1].message != "6 songs saved" {
		t.Fatalf("expected completion notification, got %+v", notices)
	}
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("background hold should last until interface reopened and closed")
	}
	a.pagePresent("second-window-open-1234")
	if !a.pageLeft("second-window-open-1234", a.sessionID) {
		t.Fatal("second close failed")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("reopen/close after completion must exit")
	}
	if len(notices) != 2 {
		t.Fatalf("idle close should not produce a background notice: %+v", notices)
	}
}

func TestCloseFlowDuplicateBrowserBeaconsAreIdempotent(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "browser-beacon-dupe-1234"
	a.pagePresent(id)
	server := a.routes()
	payload, _ := json.Marshal(map[string]string{"pageID": id, "sessionID": a.sessionID})
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/page/leave", bytes.NewReader(payload))
		req.Host = listenAddress
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("close %d: HTTP %d: %s", i+1, resp.Code, resp.Body.String())
		}
	}
	if len(a.browserPages) != 0 {
		t.Fatal("duplicate beacon re-registered a page")
	}
	badReq := httptest.NewRequest(http.MethodPost, "/api/page/leave", strings.NewReader(`{"pageID":"never-seen-page-1234","sessionID":"`+a.sessionID+`"}`))
	badReq.Host = listenAddress
	badReq.Header.Set("Content-Type", "application/json")
	badResp := httptest.NewRecorder()
	server.ServeHTTP(badResp, badReq)
	if badResp.Code != http.StatusConflict {
		t.Fatalf("unknown page must be rejected: %d", badResp.Code)
	}
}

func TestCloseFlowPausedActiveAndReviewSummary(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	var notices []capturedTrayNotice
	a.trayNotice = func(title, message string) { notices = append(notices, capturedTrayNotice{title, message}) }
	jobs := []*Job{
		{Kind: "track", Key: "active-while-paused", Title: "Still downloading"},
		{Kind: "track", Key: "reviewed-track", Title: "Needs review"},
	}
	a.addJobs(jobs)
	_, _, cancel, ok := a.nextJob()
	if !ok {
		t.Fatal("job did not start")
	}
	defer cancel()
	a.mu.Lock()
	a.state.Paused = true
	if !a.pendingDownloadsLocked() {
		t.Fatal("paused active job incorrectly treated as idle")
	}
	a.mu.Unlock()
	if got := a.queueStatus(); got != "Downloading" {
		t.Fatalf("paused active status: %q", got)
	}
	a.pagePresent("active-while-paused-tab-1234")
	if !a.pageLeft("active-while-paused-tab-1234", a.sessionID) {
		t.Fatal("page close failed")
	}
	if len(notices) != 1 || notices[0].title != "Downloading in background" {
		t.Fatalf("notification missing: %+v", notices)
	}
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("paused active download was interrupted")
	}
	// Resolve jobs without invoking the real network downloader.
	a.changeJob(jobs[0].ID, func(j *Job) { j.Status = "done" })
	a.mu.Lock()
	a.activeID = ""
	a.activeCancel = nil
	a.mu.Unlock()
	a.finishBatchAndNotify(jobs[0].ID)
	a.changeJob(jobs[1].ID, func(j *Job) { j.Status = "review" })
	a.finishBatchAndNotify(jobs[1].ID)
	if len(notices) != 2 || notices[1].title != "Done" || notices[1].message != "1 song saved · 1 need review" {
		t.Fatalf("wrong completion counts: %+v", notices)
	}
}

func TestCloseFlowIndependentLifecycleTimer(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	id := "independent-lifecycle-tab-1234"
	a.pagePresent(id)
	if !a.pageLeft(id, a.sessionID) {
		t.Fatal("page close failed")
	}
	a.mu.Lock()
	a.lastPageSeen = time.Now().Add(-browserCloseGrace - time.Second)
	a.mu.Unlock()
	var once sync.Once
	exited := make(chan struct{})
	a.shutdown = func() { once.Do(func() { a.signalShutdown(); close(exited) }) }
	go a.lifecycleLoop() // No worker loop is running.
	select {
	case <-exited:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("independent lifecycle timer did not shut down the app")
	}
}
