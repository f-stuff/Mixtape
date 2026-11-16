package main

import (
	"fmt"
	"strings"
	"time"
)

// BatchSummary counts jobs completed in THIS process run. Already-finished
// jobs from a previous run never create a spurious notification at startup.
type BatchSummary struct {
	Songs     int
	Playlists int
	Failed    int
	Review    int
}

func (s BatchSummary) notificationText() string {
	parts := []string{}
	if s.Songs > 0 {
		parts = append(parts, fmt.Sprintf("%d song%s saved", s.Songs, plural(s.Songs)))
	}
	if s.Playlists > 0 {
		parts = append(parts, fmt.Sprintf("%d playlist%s saved", s.Playlists, plural(s.Playlists)))
	}
	if s.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", s.Failed))
	}
	if s.Review > 0 {
		parts = append(parts, fmt.Sprintf("%d need review", s.Review))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Called after the worker has finished a job. Only notify after the last
// runnable job, including album tracklist expansion, has been processed.
func (a *App) recordBatchResult(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, job := range a.state.Jobs {
		if job.ID != id {
			continue
		}
		switch job.Status {
		case "done":
			if job.Kind == "track" || (job.Kind == "url" && !strings.Contains(job.URL, "/playlist?")) {
				a.batchSummary.Songs++
			} else if job.Kind == "url" {
				a.batchSummary.Playlists++
			}
		case "failed":
			a.batchSummary.Failed++
		case "review":
			a.batchSummary.Review++
		}
		break
	}
	for _, job := range a.state.Jobs {
		switch job.Status {
		case "queued", "preparing", "searching", "downloading", "tagging":
			return ""
		}
	}
	message := a.batchSummary.notificationText()
	a.batchSummary = BatchSummary{}
	return message
}

// Requires a.mu. Review jobs intentionally stay in manual review rather than
// being repeatedly matched automatically. Downloaded files are not touched.
func retryFailedLocked(s *StoredState, now time.Time) int {
	count := 0
	for _, j := range s.Jobs {
		if j.Status != "failed" {
			continue
		}
		j.Status = "queued"
		j.Progress = 0
		j.Error = ""
		j.ErrorDetail = ""
		j.Message = "Waiting to retry"
		j.UpdatedAt = now
		count++
	}
	return count
}

// finishBatchAndNotify is the single completion-notification path used by
// the worker and lifecycle tests. The message is emitted only after all
// runnable jobs (excluding skipped/review items) have left the queue.
func (a *App) finishBatchAndNotify(jobID string) {
	if message := a.recordBatchResult(jobID); message != "" {
		a.notifyTray("Done", message)
	}
}
