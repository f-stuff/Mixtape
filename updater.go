package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

const (
	updateCheckInterval  = 7 * 24 * time.Hour
	updateFailureBackoff = 24 * time.Hour
)

// ToolUpdateStatus is informational; a failed background check never marks the
// working downloader as unavailable or interrupts a download.
type ToolUpdateStatus struct {
	State       string    `json:"state"`
	Message     string    `json:"message,omitempty"`
	LastChecked time.Time `json:"lastChecked,omitempty"`
}

func updateCheckDue(now, last time.Time, success bool) bool {
	if last.IsZero() || now.Before(last) {
		return true
	}
	interval := updateFailureBackoff
	if success {
		interval = updateCheckInterval
	}
	return now.Sub(last) >= interval
}

// Auto-update is intentionally conservative: use yt-dlp's OWN stable-channel
// updater and never replace its executable while the worker is using it.
func executeYTDLPUpdate(ctx context.Context, exe string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, "--ignore-config", "--update")
	hideCommandWindow(cmd)
	output, err := cmd.CombinedOutput()
	message := stringNoHuge(strings.TrimSpace(string(output)))
	if err != nil {
		if message == "" {
			message = err.Error()
		}
		return message, fmt.Errorf("yt-dlp update failed: %w", err)
	}
	return message, nil
}

func (a *App) requestYTDLPUpdate(manual bool) error {
	a.mu.Lock()
	if a.updaterBusy || a.setupRunning || a.releaseInstalling {
		a.mu.Unlock()
		return errors.New("Audio tools are already being checked")
	}
	if a.setup.State != "ready" || !existsNonEmpty(a.ytDLP) {
		a.mu.Unlock()
		return errors.New("Finish audio tool setup before checking for updates")
	}
	// Changing a tool while a process is running (or about to run) can break
	// downloads on Windows. Automatic checks simply wait for an idle queue.
	if a.activeID != "" {
		a.mu.Unlock()
		return errors.New("Wait until the current download finishes")
	}
	for _, job := range a.state.Jobs {
		if job.Status == "queued" || job.Status == "preparing" || job.Status == "searching" || job.Status == "downloading" || job.Status == "tagging" {
			a.mu.Unlock()
			return errors.New("Wait until the download queue is idle")
		}
	}
	if !manual && !updateCheckDue(time.Now(), a.state.LastYTDLPUpdateCheck, a.state.LastYTDLPUpdateOK) {
		a.mu.Unlock()
		return nil
	}
	a.updaterBusy = true
	a.toolUpdate = ToolUpdateStatus{State: "checking", Message: "Checking yt-dlp updates…", LastChecked: a.state.LastYTDLPUpdateCheck}
	runner := a.updateCommand
	if runner == nil {
		runner = executeYTDLPUpdate
	}
	exe := a.ytDLP
	a.mu.Unlock()
	go a.finishYTDLPUpdate(runner, exe)
	return nil
}

func (a *App) finishYTDLPUpdate(runner func(context.Context, string) (string, error), exe string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := runner(ctx, exe)
	now := time.Now()
	a.mu.Lock()
	a.state.LastYTDLPUpdateCheck = now
	a.state.LastYTDLPUpdateOK = err == nil
	if err != nil {
		a.toolUpdate = ToolUpdateStatus{State: "error", Message: "Update check failed; existing downloader kept", LastChecked: now}
	} else {
		message := "Downloader is up to date"
		if strings.Contains(strings.ToLower(output), "updated yt-dlp") || strings.Contains(strings.ToLower(output), "updating to") {
			message = "Downloader updated"
		}
		a.toolUpdate = ToolUpdateStatus{State: "ready", Message: message, LastChecked: now}
	}
	if e := a.saveLocked(); e != nil {
		log.Printf("Cannot save yt-dlp update check: %v", e)
	}
	a.updaterBusy = false
	a.mu.Unlock()
	if err != nil {
		log.Printf("yt-dlp update: %v: %s", err, output)
	}
}

func (a *App) autoUpdateLoop() {
	// Run at startup (once setup is ready) and retry idle checks periodically.
	// A successful check is stored in queue.json and is due again in a week.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		_ = a.requestYTDLPUpdate(false) // Busy/setup/not-yet-due is normal.
		select {
		case <-a.stopping:
			return
		case <-ticker.C:
		}
	}
}
