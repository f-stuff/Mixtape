package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPauseQueuePersistsAndFinishesActiveJob(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Key: "first", Kind: "url"}, {Key: "second", Kind: "url"}})
	h := a.routes()
	if result := testPost(t, h, "/api/queue/pause", map[string]bool{"paused": true}, true); result.Code != http.StatusOK {
		t.Fatalf("Pause failed: %s", result.Body.String())
	}
	if _, _, _, ok := a.nextJob(); ok {
		t.Fatal("A paused queue should not start jobs")
	}
	reopened, err := newApp(root)
	if err != nil || !reopened.state.Paused {
		t.Fatalf("Pause did not survive restart: %v", err)
	}
	if result := testPost(t, h, "/api/queue/pause", map[string]bool{"paused": false}, true); result.Code != http.StatusOK {
		t.Fatalf("Resume failed: %s", result.Body.String())
	}
	first, ctx, cancel, ok := a.nextJob()
	if !ok || first.Key != "first" {
		t.Fatal("Resume should start the oldest queued job")
	}
	defer cancel()
	if result := testPost(t, h, "/api/queue/pause", map[string]bool{"paused": true}, true); result.Code != http.StatusOK {
		t.Fatalf("Pause during active job failed: %s", result.Body.String())
	}
	select {
	case <-ctx.Done():
		t.Fatal("Pausing must not cancel the active job")
	default:
	}
	a.changeJob(first.ID, func(j *Job) { j.Status = "done" })
	cancel()
	a.mu.Lock()
	a.activeID, a.activeCancel = "", nil
	a.mu.Unlock()
	if _, _, _, ok := a.nextJob(); ok {
		t.Fatal("Paused queue started a second item")
	}
	if result := testPost(t, h, "/api/queue/pause", map[string]bool{"paused": false}, true); result.Code != http.StatusOK {
		t.Fatal("Could not resume second time")
	}
	second, _, cancelSecond, ok := a.nextJob()
	if !ok || second.Key != "second" {
		t.Fatal("Second queued item did not resume")
	}
	cancelSecond()
}

func TestRemovingJustClaimedJobCancelsIt(t *testing.T) {
	a, err := newApp(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a.setup.State = "ready"
	a.addJobs([]*Job{{Key: "first", Kind: "url", URL: "https://youtu.be/abcdefg1234"}})
	job, ctx, cancel, ok := a.nextJob()
	if !ok || a.activeCancel == nil {
		t.Fatal("A claimed job must have cancellation installed immediately")
	}
	defer cancel()
	result := testPost(t, a.routes(), "/api/job/remove", map[string]string{"id": job.ID}, true)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "true") {
		t.Fatalf("Remove failed: %d %s", result.Code, result.Body.String())
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Removing the claimed job did not cancel it")
	}
}

func TestFailureMessagesAreUsefulAndBounded(t *testing.T) {
	cases := []struct{ errorText, expected string }{
		{"yt-dlp failed: ERROR: HTTP Error 429: Too Many Requests", "limiting requests"},
		{"ERROR: Video unavailable", "isn't available"},
		{"open file: Access is denied", "permissions"},
		{"write audio: no space left on device", "drive is full"},
		{"yt-dlp failed: request timed out", "Network problem"},
		{"MP3 tagging failed: invalid data", "MP3 tags"},
		{"yt-dlp failed: ERROR: Unknown issue", "Unknown issue"},
	}
	for _, tc := range cases {
		summary, detail := jobFailure(errors.New(tc.errorText))
		if !strings.Contains(summary, tc.expected) || detail != tc.errorText {
			t.Errorf("Unexpected error mapping for %q: %q / %q", tc.errorText, summary, detail)
		}
	}
	if summary, detail := jobFailure(nil); summary != "" || detail != "" {
		t.Fatal("nil error must produce no message")
	}
	long := strings.Repeat("🙂", 5000)
	summary, detail := jobFailure(fmt.Errorf("ERROR: %s", long))
	if len([]rune(detail)) > 4000 || len([]rune(summary)) > 201 || !strings.HasSuffix(detail, "🙂") {
		t.Fatal("Failed to bound long technical output safely")
	}
}

func TestErrorDetailSurvivesRestartAndClearsOnRetry(t *testing.T) {
	root := t.TempDir()
	a, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	a.addJobs([]*Job{{Key: "failed-song", Kind: "track", Title: "Example"}})
	id := a.state.Jobs[0].ID
	summary, detail := jobFailure(errors.New("yt-dlp failed: ERROR: Video unavailable; provider code 299"))
	a.changeJob(id, func(j *Job) { j.Status = "failed"; j.Error = summary; j.ErrorDetail = detail })
	b, err := newApp(root)
	if err != nil {
		t.Fatal(err)
	}
	j, _ := b.jobCopy(id)
	if j.ErrorDetail != detail || j.Error != summary {
		t.Fatalf("Lost persisted diagnostics: %+v", j)
	}
	r := testPost(t, b.routes(), "/api/job/retry", map[string]string{"id": id}, true)
	if r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	j, _ = b.jobCopy(id)
	if j.Status != "queued" || j.Error != "" || j.ErrorDetail != "" {
		t.Fatalf("Retry left stale errors: %+v", j)
	}
	b.changeJob(id, func(j *Job) { j.Status = "failed"; j.Error = summary; j.ErrorDetail = detail })
	r = testPost(t, b.routes(), "/api/queue/retry-failed", map[string]any{}, true)
	if r.Code != http.StatusOK {
		t.Fatal(r.Body.String())
	}
	j, _ = b.jobCopy(id)
	if j.Status != "queued" || j.ErrorDetail != "" {
		t.Fatal("Retry all should clear detailed errors as well")
	}
}

func TestDownloadFailureKeepsTechnicalOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh; Windows build is compiled separately")
	}
	root := t.TempDir()
	script := filepath.Join(root, "fake-yt-dlp")
	content := "#!/bin/sh\nprintf '%s\\n' 'WARNING: testing a recoverable condition' 'ERROR: HTTP Error 429: Too Many Requests' >&2\nexit 1\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	a.ytDLP = script
	a.addJobs([]*Job{{Kind: "url", Key: "error-example", URL: "https://youtu.be/abcdefg1234"}})
	job := *a.state.Jobs[0]
	_, runErr := a.runDownload(context.Background(), job, "url", filepath.Join(root, "song.%(ext)s"))
	if runErr == nil {
		t.Fatal("Failed mock downloader incorrectly reported success")
	}
	summary, detail := jobFailure(runErr)
	if !strings.Contains(summary, "limiting requests") || !strings.Contains(detail, "429") || !strings.Contains(detail, "WARNING:") {
		t.Fatalf("Lost actionable stderr: %q / %q", summary, detail)
	}
}
