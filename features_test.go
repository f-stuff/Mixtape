package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryFailedBulkKeepsReviewAndFinished(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	a.addJobs([]*Job{
		{Kind: "track", Key: "one", Title: "One"},
		{Kind: "url", Key: "two", Title: "Two"},
		{Kind: "track", Key: "three", Title: "Three"},
		{Kind: "url", Key: "four", Title: "Four"},
	})
	ids := []string{}
	for _, j := range a.state.Jobs {
		ids = append(ids, j.ID)
	}
	a.changeJob(ids[0], func(j *Job) { j.Status = "failed"; j.Error = "connection failure"; j.Progress = 81 })
	a.changeJob(ids[1], func(j *Job) { j.Status = "failed"; j.Error = "temporarily unavailable" })
	a.changeJob(ids[2], func(j *Job) { j.Status = "review"; j.Message = "Choose a recording" })
	a.changeJob(ids[3], func(j *Job) { j.Status = "done"; j.Progress = 100 })

	h := a.routes()
	if r := testPost(t, h, "/api/queue/retry-failed", map[string]any{}, false); r.Code != http.StatusForbidden {
		t.Fatalf("Missing request header got %d", r.Code)
	}
	r := testPost(t, h, "/api/queue/retry-failed", map[string]any{}, true)
	if r.Code != http.StatusOK {
		t.Fatalf("Retry all failed got %d: %s", r.Code, r.Body.String())
	}
	var result struct {
		Retried int `json:"retried"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &result); err != nil || result.Retried != 2 {
		t.Fatalf("Expected 2 retried: %+v (%v)", result, err)
	}
	for i, expect := range []string{"queued", "queued", "review", "done"} {
		j, _ := a.jobCopy(ids[i])
		if j.Status != expect {
			t.Errorf("Job %d status %s, expected %s", i, j.Status, expect)
		}
		if i < 2 && (j.Error != "" || j.Progress != 0 || j.Message != "Waiting to retry") {
			t.Errorf("Job %d did not reset cleanly: %+v", i, j)
		}
	}
	if r = testPost(t, h, "/api/queue/retry-failed", map[string]any{}, true); !strings.Contains(r.Body.String(), `"retried":0`) {
		t.Fatalf("Retrying twice should have no additional jobs: %s", r.Body.String())
	}
	reopened, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:2] {
		j, ok := reopened.jobCopy(id)
		if !ok || j.Status != "queued" {
			t.Errorf("Retry did not persist after restart: %+v", j)
		}
	}
}

func TestBatchNotificationsOnceWhenQueueIsIdle(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.addJobs([]*Job{
		{Kind: "album", Key: "album"},
		{Kind: "track", Key: "track"},
		{Kind: "url", Key: "playlist", URL: "https://www.youtube.com/playlist?list=123456"},
		{Kind: "url", Key: "url", URL: "https://youtu.be/abcdefg1234"},
	})
	jobs := a.state.Jobs
	for i, job := range jobs {
		status := "done"
		if i == 3 {
			status = "failed"
		}
		a.changeJob(job.ID, func(j *Job) { j.Status = status })
		message := a.recordBatchResult(job.ID)
		if i < 3 && message != "" {
			t.Fatalf("Notified before all jobs completed: %q", message)
		}
		if i == 3 && message != "1 song saved · 1 playlist saved · 1 failed" {
			t.Fatalf("Bad batch summary: %q", message)
		}
	}
	if message := (BatchSummary{Failed: 2}).notificationText(); message != "2 failed" {
		t.Fatal(message)
	}
	if message := (BatchSummary{}).notificationText(); message != "" {
		t.Fatal("Empty batches must not notify")
	}
}

func TestUpdateCheckDue(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	if !updateCheckDue(now, time.Time{}, false) || !updateCheckDue(now, now.Add(time.Minute), true) {
		t.Fatal("Never/invalid future check date should be considered due")
	}
	if updateCheckDue(now, now.Add(-6*24*time.Hour), true) || !updateCheckDue(now, now.Add(-8*24*time.Hour), true) {
		t.Fatal("Successful update checks must wait a week")
	}
	if updateCheckDue(now, now.Add(-22*time.Hour), false) || !updateCheckDue(now, now.Add(-25*time.Hour), false) {
		t.Fatal("Failed update checks should retry after a day")
	}
}

func TestManualUpdaterIdleGateAndPersistence(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	fakeExecutable := filepath.Join(root, "yt-dlp.exe")
	if err := os.WriteFile(fakeExecutable, []byte(strings.Repeat("x", 2048)), 0600); err != nil {
		t.Fatal(err)
	}
	a.ytDLP = fakeExecutable
	a.setup.State = "ready"
	started := make(chan struct{})
	finish := make(chan struct{})
	a.updateCommand = func(_ context.Context, executable string) (string, error) {
		if executable != fakeExecutable {
			return "", errors.New("wrong executable")
		}
		close(started)
		<-finish
		return "Updated yt-dlp to new version", nil
	}
	a.addJobs([]*Job{{Kind: "track", Key: "pending"}})
	if err := a.requestYTDLPUpdate(true); err == nil {
		t.Fatal("Updater must not run while downloads are queued")
	}
	id := a.state.Jobs[0].ID
	a.changeJob(id, func(j *Job) { j.Status = "done" })
	if err := a.requestYTDLPUpdate(true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Updater did not start")
	}
	a.addJobs([]*Job{{Kind: "track", Key: "new-job"}})
	if _, _, _, ok := a.nextJob(); ok {
		t.Fatal("Download started while yt-dlp update was running")
	}
	close(finish)
	deadline := time.After(2 * time.Second)
	for {
		a.mu.Lock()
		busy, status := a.updaterBusy, a.toolUpdate
		a.mu.Unlock()
		if !busy {
			if status.State != "ready" || status.Message != "Downloader updated" {
				t.Fatalf("Unexpected updater result: %+v", status)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("Updater did not complete")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, _, cancel, ok := a.nextJob(); !ok {
		t.Fatal("Expected a job after the updater completed")
	} else {
		cancel()
	}
	reopened, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.LastYTDLPUpdateCheck.IsZero() || !reopened.state.LastYTDLPUpdateOK {
		t.Fatal("Updated status was not saved across restarts")
	}
	if updateCheckDue(time.Now(), reopened.state.LastYTDLPUpdateCheck, reopened.state.LastYTDLPUpdateOK) {
		t.Fatal("Freshly checked update should not run again automatically")
	}
}

func TestUpdateEndpointRejectsBusyQueue(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	if r := testPost(t, h, "/api/tools/update", map[string]any{}, false); r.Code != http.StatusForbidden {
		t.Fatalf("No auth header should be rejected: %d", r.Code)
	}
	if r := testPost(t, h, "/api/tools/update", map[string]any{}, true); r.Code != http.StatusConflict {
		t.Fatalf("Updater should report not ready: %d", r.Code)
	}
}
