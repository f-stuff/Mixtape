package main

// queueStatusLocked is shared by the browser and the native Windows tray.
// Skipped tracks and already-prepared album headers are not unfinished work.
// Caller must hold a.mu.
func (a *App) queueStatusLocked() string {
	switch a.setup.State {
	case "error":
		return "Setup needed"
	case "ready":
	default:
		return "Setting up"
	}
	hasQueued, hasActive, hasReview, hasDone := false, a.activeID != "", false, false
	for _, job := range a.state.Jobs {
		switch job.Status {
		case "preparing", "searching", "downloading", "tagging":
			hasActive = true
		case "queued":
			hasQueued = true
		case "failed", "review":
			hasReview = true
		case "done":
			if job.Kind == "track" || job.Kind == "url" {
				hasDone = true
			}
		}
	}
	switch {
	case hasActive:
		return "Downloading"
	case a.state.Paused:
		return "Queue paused"
	case hasQueued:
		return "Queued"
	case hasReview:
		return "Needs review"
	case hasDone:
		return "Finished"
	default:
		return "Ready"
	}
}

func (a *App) queueStatus() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.queueStatusLocked()
}
