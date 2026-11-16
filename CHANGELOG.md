# Mixtape changelog

## 1.4.1 — reliable browser shutdown and tray notifications

- **Lifecycle fix:** EventSource connection/reconnection events no longer update the last-browser-close timestamp. Only registered browser tabs and their close/heartbeat events determine whether a page is open.
- **Independent shutdown monitor:** Every second, checks whether the last tab has closed, the three-second grace period has passed, and no active download, setup, or updater operation requires Mixtape to remain running. Browser tab leases expire after 30 seconds if Firefox misses close events.
- **Firefox close reliability:** The page sends both a `sendBeacon` and a `fetch(..., {keepalive: true})` close request. Duplicate close requests are accepted without affecting other tabs.
- **Background notifications:** Closing the final page while selected songs are pending shows **Mixtape - Downloading in background**. The app keeps processing; when the last runnable job finishes, **Mixtape - Done** summarises saved songs, playlists, failed downloads, and tracks needing manual review. A late background notification is suppressed if work has already finished.
- **Reopen and close:** After the background batch finishes, reopening Mixtape from the tray/notification then closing the final browser tab allows the EXE to exit. Six completed tracks plus six skipped tracks count as finished immediately, with no background hold.
- **Paused active download:** Pausing prevents the next song from starting but never treats an already-active song as idle. The browser and tray show **Downloading** until that song finishes.
- **Tray notification click:** Clicking a Windows notification opens Mixtape, just like clicking the tray icon.
- **GitHub updater retained:** Keeps the GitHub Releases check, verified release ZIP download, separate post-exit installer, and saved queue/music preservation from the prior v1.4 patch.
- Added focused `close_flow_test.go` regression tests; updated release-version tests for the compiled v1.4.1 build.

## 1.4.0 — GitHub app updater follow-up

- Checks the project's **public GitHub Releases** for newer stable versions on launch and daily while Mixtape remains open.
- Shows one small **Update & restart** action when an updated portable Windows ZIP is available. Settings also offers **Check for updates**. Updates are never installed automatically during an active queue.
- Downloads the selected GitHub release ZIP, validates its published SHA-256 and size, extracts only supported application files, waits for Mixtape to exit, applies the update using an embedded Windows PowerShell helper, and relaunches Mixtape.
- Keeps the music folder, `%LOCALAPPDATA%/Mixtape/queue.json`, skipped songs, and existing download helpers. Existing app files are backed up during copying and restored if replacement fails.
- Works with either of the two build-script ZIP filenames; newer releases require a new increasing stable tag (`v1.4.1` etc.).

## 1.4.0 — automatic updates, notifications, retry and queue controls

- **Tray status + auto-exit follow-up:** The native tray tooltip and the disabled status line in its right-click menu now use the **same backend queue status as the browser**. Six downloaded songs plus six unchecked songs show **Finished** everywhere; the stale static "running in background" label is removed. The status refreshes automatically while the tray is open.
- **Firefox close reliability:** Send the browser close beacon from both `beforeunload` and `pagehide` (only once per visit); restore a fresh tab ID after back/forward restoration. If a browser drops both events, a missing page heartbeat expires after 45 seconds, then the normal 9-second close grace applies. Existing selected downloads still finish in the tray, and the user can reopen and close Mixtape to exit afterward.
- **Final idle shutdown fix:** Six completed tracks and six unchecked tracks now count as **Finished**. Closing the final page exits after a brief grace period when no selected work is running. The header distinguishes Downloading, Queued, Finished and Ready rather than always saying Running.
- **Reliable Firefox tab detection:** Browser tabs now have distinct IDs and presence heartbeats. A pagehide beacon removes the tab immediately; expired heartbeats recover from missed browser shutdown events. A stale event-stream socket or a late polling response can no longer keep the tray running.
- **Finish in background:** Closing the page while selected tracks are still processing keeps Mixtape alive to complete the batch and show the completion notification. Opening and closing the page again after completion exits normally. Skipped/failed/review tracks do not keep the queue active.
- Metadata artist, album and tracklist requests now **retry temporary MusicBrainz failures twice** with backoff; they retain the one-request-per-second throttle and still fail immediately for permanent errors.
- **Song checkboxes** within queued albums: uncheck to skip (including canceling a currently active song), check again to put that track back in the queue. Skips persist across restarts. Completed MP3s stay on disk and cannot be removed through the checkboxes.
- **Automatic idle exit:** Closing an idle page exits after a short grace period. Closing mid-download lets the batch finish in the tray and leaves it available until the user reopens and closes it again. All queue entries remain saved, including skipped songs; multiple pages and reloads are handled.
- Checks for official **stable yt-dlp updates** approximately once a week, only when no download is queued or running. Failed checks are retried the next day. The last check is persisted across restarts; an update error does not stop downloads.
- **Settings → Check now** lets you request the same update immediately when the queue is idle. The first-time setup repair button remains separate.
- When the current batch of queued work finishes, a **Windows tray notification** summarises saved songs, completed playlist jobs, and failures. Respects Windows notification settings. No new runtime or installer.
- **Retry failed (N)** in My queue re-queues every failed job in one click; tracks awaiting review and previously saved files are left alone. The button is hidden when there are no failures.
- Added automated tests for retry persistence, updater cooldown and idle-queue protection, and batch notification summaries.
- **Pause / Resume** is clearly labelled in My queue with a visible paused-state message. Pausing is saved and does not interrupt the current item; subsequent items wait until Resume, including after a restart.
- Failed albums, songs, and links now show a readable cause, expandable **Technical details**, and **Copy error** for sharing diagnostics. Old failed jobs still show their original message. Retrying clears obsolete details.
- Fixed cancellation when a queued item is removed immediately after the worker claims it, and corrected album failure badges when only child tracks fail.
- Moved visible toast notifications below the top bar so they no longer cover **Open music folder**.
- Expanded tests cover pause persistence, active-item cancellation, downloader stderr capture, full failure-detail persistence and clearing, plus the queue controls in a headless browser.


## 1.3.0 — default browser and shutdown detection

- Mixtape opens the dashboard through Windows' **default browser association**, including Firefox, instead of selecting Edge or Chrome. The extra browser-profile code is removed.
- On Quit, the localhost backend sends a shutdown event to each open Mixtape tab. The page attempts to close itself; if the browser blocks this (normal in Firefox), it shows a tidy "Mixtape is closed" page.
- If the backend crashes or restarts, polling detects its disappearance, with a distinct session ID to recognise a new instance. Transient connection errors are tolerated before declaring it closed.
- The tray icon and existing Browse folder, open-location, and toast fixes are unchanged.


## 1.2.0 — folder picker, notifications, browser shutdown

- Settings → Download folder now includes **Browse…**, which launches the Windows-native folder selection dialog. Choosing a path fills the box; press **Save settings** to use it.
- The toast message is truly hidden when inactive and does not leave a partial strip near the top edge. Notifications disappear completely after the timeout.
- On Windows, Mixtape now opens an app-style Edge window (Chrome if Edge is unavailable) with its own local browser profile. Closing that window still leaves downloads running in the tray.
- **Quit Mixtape** from either the tray or Settings closes Mixtape's owned window, stops the background app, and removes the tray icon. Reopening from the tray raises the same window, avoiding extra tabs.
- Mixtape does not close unrelated browser windows or tabs. On systems without Edge or Chrome, the fallback default-browser tab can't safely be closed on Quit.
- Updated tests for folder-picker requests, and documentation for the new browser lifecycle.

## 1.1.0 — tray and Explorer actions

- Native Windows tray icon, simpler interface, functioning music-folder opener, and per-album/per-track Explorer actions.
