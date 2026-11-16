package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTrayAndPageShareFinishedStatusForSixOfTwelve(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Kind: "album", Key: "album:one", Title: "Album", Album: "Album"}})
	parent := a.state.Jobs[0].ID
	a.changeJob(parent, func(j *Job) { j.Status = "done" })
	for i := 0; i < 12; i++ {
		key := fmt.Sprintf("track:%d", i)
		a.addJobs([]*Job{{Kind: "track", Key: key, ParentID: parent, Title: key}})
		id := a.state.Jobs[len(a.state.Jobs)-1].ID
		if i < 6 {
			a.changeJob(id, func(j *Job) { j.Status = "done" })
		} else if _, err := a.setTrackEnabled(id, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := a.queueStatus(); got != "Finished" {
		t.Fatalf("Tray has wrong state for six done/six skipped: %q", got)
	}
	id := "real-user-browser-tab-1234"
	req := httptest.NewRequest(http.MethodGet, "http://"+listenAddress+"/api/state", nil)
	req.Header.Set("X-Mixtape-Page", id)
	res := httptest.NewRecorder()
	a.routes().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("Page state HTTP %d: %s", res.Code, res.Body.String())
	}
	var state struct {
		QueueStatus string `json:"queueStatus"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.QueueStatus != a.queueStatus() {
		t.Fatalf("Page %q and tray %q disagree", state.QueueStatus, a.queueStatus())
	}
	if !a.pageLeft(id, a.sessionID) {
		t.Fatal("Browser close not accepted")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Finished album should allow tray process to exit on close")
	}
	// Rechecking an unchecked song makes both surfaces show queued again.
	requeueID := a.state.Jobs[7].ID
	if _, err := a.setTrackEnabled(requeueID, true); err != nil {
		t.Fatal(err)
	}
	if got := a.queueStatus(); got != "Queued" {
		t.Fatalf("Re-enabled song should be queued, got %q", got)
	}
}

func TestIdleExitAfterMissedFirefoxBeacon(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.pageConnected() // A stale EventSource socket must not hold the process open.
	a.pagePresent("firefox-dropped-tab-1234")
	if a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("A live page must not close the app")
	}
	// Simulate a Firefox close where neither sendBeacon nor beforeunload
	// was delivered. A missed heartbeat eventually expires the page lease.
	a.mu.Lock()
	a.browserPages["firefox-dropped-tab-1234"] = time.Now().Add(-pageLeaseTimeout - time.Second)
	a.mu.Unlock()
	if a.canExitAfterClosingPage(time.Now()) {
		t.Fatal("Expired lease should still get its closing grace period")
	}
	if !a.canExitAfterClosingPage(time.Now().Add(browserCloseGrace + time.Second)) {
		t.Fatal("Abandoned browser tab should eventually release the tray")
	}
}

func TestTrayQueueStatusDistinguishesActualDownloadFromSkipped(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	if got := a.queueStatus(); got != "Ready" {
		t.Fatalf("empty queue: %q", got)
	}
	a.addJobs([]*Job{{Kind: "track", Key: "track:sample", Title: "Sample"}})
	id := a.state.Jobs[0].ID
	if got := a.queueStatus(); got != "Queued" {
		t.Fatalf("queued track: %q", got)
	}
	job, _, cancel, ok := a.nextJob()
	if !ok || job.ID != id {
		t.Fatal("queued track did not start")
	}
	if got := a.queueStatus(); got != "Downloading" {
		t.Fatalf("active track: %q", got)
	}
	cancel()
	a.mu.Lock()
	a.activeID = ""
	a.activeCancel = nil
	a.mu.Unlock()
	if _, err := a.setTrackEnabled(id, false); err != nil {
		t.Fatal(err)
	}
	if got := a.queueStatus(); got != "Ready" {
		t.Fatalf("skipped-only queue should not be in progress: %q", got)
	}
}
