package main

import (
	"regexp"
	"time"
)

// Every browser tab has its own ephemeral ID. A pagehide beacon removes it
// immediately; a lease allows the app to recover if a browser crashes or
// silently drops the page without closing an EventSource connection.
const (
	pageLeaseTimeout      = 30 * time.Second
	departedPageRetention = 5 * time.Minute
)

var pageIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{12,80}$`)

func validPageID(id string) bool { return pageIDPattern.MatchString(id) }

// a.mu must be held.
func (a *App) pendingDownloadsLocked() bool {
	// Pause only prevents new jobs. The already-started download still runs.
	if a.activeID != "" {
		return true
	}
	if a.state.Paused || a.setup.State == "error" {
		return false
	}
	for _, job := range a.state.Jobs {
		switch job.Status {
		case "queued", "preparing", "searching", "downloading", "tagging":
			return true
		}
	}
	return false
}

// a.mu must be held. Called only after the last page has gone away.
func (a *App) lastBrowserPageClosedLocked(now time.Time) bool {
	a.lastPageSeen = now
	// If there is runnable work, keep the tray alive even after completion,
	// so the user can see the Done notification and reopen the window.
	a.holdAfterBackground = a.pendingDownloadsLocked()
	return a.holdAfterBackground
}

func (a *App) pagePresent(id string) {
	if !validPageID(id) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.browserPages == nil {
		a.browserPages = make(map[string]time.Time)
	}
	if a.departedPages == nil {
		a.departedPages = make(map[string]time.Time)
	}
	for oldID, departedAt := range a.departedPages {
		if time.Since(departedAt) > departedPageRetention {
			delete(a.departedPages, oldID)
		}
	}
	if _, alreadyLeft := a.departedPages[id]; alreadyLeft {
		return // A late in-flight poll must not revive a tab after pagehide.
	}
	if len(a.browserPages) == 0 {
		// Reopening the interface is a new interaction. It clears the hold
		// from a previous background batch, without stopping current jobs.
		a.holdAfterBackground = false
	}
	a.browserPages[id] = time.Now()
	a.pageWasOpened = true
}

func (a *App) pageLeft(id string, session string) bool {
	a.mu.Lock()
	if session != a.sessionID || !validPageID(id) {
		a.mu.Unlock()
		return false
	}
	if _, ok := a.browserPages[id]; !ok {
		// Both sendBeacon and keepalive fetch may deliver the same close.
		// Accept duplicates, but never remove a different tab.
		_, alreadyLeft := a.departedPages[id]
		a.mu.Unlock()
		return alreadyLeft
	}
	delete(a.browserPages, id)
	if a.departedPages == nil {
		a.departedPages = make(map[string]time.Time)
	}
	a.departedPages[id] = time.Now()
	backgroundNotice := false
	if len(a.browserPages) == 0 {
		backgroundNotice = a.lastBrowserPageClosedLocked(time.Now())
	}
	a.mu.Unlock()
	if backgroundNotice {
		a.notifyTray("Downloading in background", "Mixtape will finish the selected songs.")
	}
	return true
}

// a.mu must be held. Fallback when pagehide is not delivered, e.g. Firefox
// or Windows crashes. The normal close path is the beacon, not this timeout.
func (a *App) expireBrowserPagesLocked(now time.Time) bool {
	if len(a.browserPages) == 0 {
		return false
	}
	removed := false
	if a.departedPages == nil {
		a.departedPages = make(map[string]time.Time)
	}
	for id, lastSeen := range a.browserPages {
		if now.Sub(lastSeen) > pageLeaseTimeout {
			delete(a.browserPages, id)
			a.departedPages[id] = now // Ignore late polls and duplicate close signals.
			removed = true
		}
	}
	if removed && len(a.browserPages) == 0 {
		return a.lastBrowserPageClosedLocked(now)
	}
	return false
}
