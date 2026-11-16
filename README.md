# Mixtape

**Portable Windows music collection app.**

Mixtape is a single Windows GUI executable with an embedded web interface. Double-click it; Mixtape opens its locally hosted dashboard in your Windows **default browser** (Firefox, Edge, Chrome, etc.). Search artists, select entire albums, queue YouTube / YouTube Music URLs, and return to artist/album-organised MP3s.

## Features in v1.4.1

- Search artists and browse studio albums via the MusicBrainz API. Temporary lookup failures (busy server, timeout or bad gateway) are retried automatically up to two more times, respecting the API rate limit.
- Tick multiple albums. The background worker looks up an album's official tracklist and searches YouTube for corresponding recordings. **Open the album in My queue and uncheck unwanted songs**; they are saved as Skipped and won't download. Check them again to put them back in the queue. Skipping an in-progress song cancels that job. Already downloaded MP3s are never deleted by a checkbox.
- Conservative scoring uses title, artist/channel, and track duration. Ambiguous matches go to **Needs review** with suggested recordings and an option to paste the right URL.
- Batch video URLs plus YouTube playlist URLs.
- Convert best available source audio into V0-quality variable-bitrate MP3 with yt-dlp and FFmpeg. Transcoding never restores source quality.
- Organise songs under `Music/Mixtape/Artist/Album (Year)/NN - Title.mp3` with album/artist/title/year/track ID3 tags, plus album covers where available.
- Persistent JSON queue (in `%LOCALAPPDATA%/Mixtape/queue.json`), duplicate detection, retries, and background operation while jobs are actively processing. **Closing the last page while idle exits Mixtape after approximately three seconds.** Six downloaded songs plus six skipped songs count as finished. Closing while selected downloads are still running lets the batch finish in the tray and sends a completion notification; reopen and close the page afterward to exit. Nothing in the queue is discarded. A paused queue is persisted and may quit instead of staying in the tray. **Pause queue / Resume queue** stops new jobs from starting while allowing the current job to finish; the pause setting survives restarts. Failed items have a short explanation, expandable technical details and a **Copy error** button. **Retry failed (N)** re-queues failures without re-running tracks awaiting manual review.
- A native Windows tray icon shows the same current queue status as the web page (Finished, Downloading, etc.), and lets you open Mixtape, open the music folder, or quit. Windows shows **Downloading in background** after the last page closes with selected work pending, then **Done** after the batch finishes, summarising saved songs/playlists, failures and tracks needing review (subject to Windows notification settings). On Quit, the page attempts to close itself and shows a small closed message if the browser blocks it.
- **Automatic yt-dlp update checks:** on launch and while the app stays open, Mixtape checks about once a week, only while the queue is idle. An unsuccessful check is retried after a day. You can check immediately via **Settings → Check now**. No new installer or library is required.
- **Mixtape app updates via GitHub Releases:** on launch and daily while open, checks `f-stuff/Mixtape` for a newer stable tag. A small notice offers **Update & restart** once the queue is idle. Mixtape verifies the GitHub-provided ZIP SHA-256, stages the update, exits, replaces application files through Windows PowerShell, and reopens itself. The saved queue, chosen music folder, and existing media helpers are preserved. **Settings → Mixtape updates → Check for updates** runs the check manually. The app never updates itself silently in the middle of a download.
- The download directory has a **Browse…** button with a native Windows folder chooser. Toast notifications stay hidden when inactive.
- A normal user can run it without admin permission or installing Python, Go, FFmpeg, yt-dlp, or Deno. Required helper tools are fetched automatically on first launch.

## Files

- `main.go`: local-only HTTP server, JSON queue, settings and API.
- `musicbrainz.go`: metadata API and album tracklists (throttled to one request per second; bounded retries on temporary failures).
- `matcher.go`: recording matching and conservative review threshold.
- `worker.go`: background queue, YouTube search/downloads and ID3 metadata.
- `tools.go`: initial downloads of portable yt-dlp, FFmpeg, and Deno binaries.
- `updater.go`: weekly, idle-only yt-dlp updates through yt-dlp's official stable self-updater; persists the last check in `queue.json`.
- `github_updates.go`: fetches the latest published GitHub release, checks versions/asset hashes, stages a safe update and starts the post-exit helper.
- `scripts/apply-update.ps1` and `release_installer_windows.go`: replace the portable EXE and release docs after the app has exited, then relaunch Mixtape. Existing `tools` binaries are kept rather than downgraded.
- `batch.go`: queue completion summaries and bulk retry logic.
- `errors.go`: friendly error summaries and bounded technical diagnostics for Copy error.
- `platform_windows.go` / `platform_other.go`: Windows shell integration, reliable Explorer folder opening, sleep prevention.
- `folder_picker_windows.go`: native Windows directory chooser (no installer or PowerShell).
- `tray_windows.go` / `tray_other.go` and `assets/mixtape.ico`: native Windows notification-area icon, menu, and completion notifications, embedded in the EXE (no external tray helper).
- `web/app.js`, `/api/lifecycle` and `/api/page/leave`: per-tab presence heartbeats, dual last-tab close requests (beacon + keepalive fetch), and shutdown detection separate from stale Firefox EventSource connections.
- `browser_presence.go`: presence leases independent of stale Firefox event streams, background completion handling, and guards against late browser requests.
- `web/`: HTML/CSS/JavaScript, embedded into the executable by Go.
- `main_test.go`, `features_test.go`, `final_features_test.go`, `finishing14_test.go`: tests for queue, updates, per-song selection, metadata retries, page-close exit and batch handling, plus a local fake-yt-dlp pipeline test.

## Build and revise

**Windows one-click builders are included in the source ZIP.** Double-click `build.bat` to run tests and produce an updated `Mixtape.exe` plus lightweight ZIP, `build-portable.bat` to package yt-dlp/FFmpeg/Deno into a larger ZIP, or `test.bat` to run tests only. On a developer PC without Go 1.23+, these scripts download and verify the official Go compiler in `.build-cache` automatically; no admin rights or global Go installation are required. See [BUILDING.md](BUILDING.md) for the complete workflow and editing guide.

For command-line builds, Go 1.23+ is required:

```bash
# Linux host cross-compiling a Windows GUI executable:
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w -H windowsgui' -o Mixtape.exe .

# Unit tests (Linux includes an actual FFmpeg tagging test):
go test ./... -v
```

Run `Mixtape.exe` on Windows 10/11 x64. There is **no Go/Python runtime dependency** for the user. The executable serves `http://127.0.0.1:43875/` and opens it through Windows' registered default browser. Reopening the executable while Mixtape is running opens the page again in that browser (potentially in a new tab).

## Portable variants

1. **Lightweight self-bootstrapping ZIP (provided):** contains the 6–7 MB `Mixtape.exe` plus instructions. The app downloads yt-dlp, Deno and FFmpeg into `%LOCALAPPDATA%/Mixtape/tools` automatically on first launch (~170 MB). After first-time setup it needs no reinstall.
2. **Fully pre-bundled ZIP (build script included):** Double-click `build-portable.bat` on Windows, or run `.github/workflows/full-portable.yml` in GitHub Actions. Both download the portable executable dependencies and package `Mixtape.exe` and `tools/`. That version can skip downloading those helper binaries on first launch, although online music search/downloads and EJS challenge scripts may still need network access.

## Background operation

The app runs a single background worker and checks the queue automatically. When you close the **last** Mixtape browser page, **an active download continues in the tray**. The worker carries on through the selected songs and shows the Windows completion notification. If you **closed the page during downloading**, Mixtape stays available in the tray so you can reopen it to see the results; **closing it again after the batch finishes** exits after approximately nine seconds. Firefox tabs send a close signal on both browser exit events; when the browser drops both, Mixtape falls back to expiry of its inactive-page heartbeat (up to roughly 45 extra seconds). If no selected work is active **when you close the last page**, Mixtape exits after the same grace period, including when some songs are skipped, failed or waiting for review. All jobs remain saved for the next launch. Closing one of several open Mixtape tabs does not quit the app. A paused queue is persisted and lets Mixtape exit when the current active job finishes; reopening the EXE resumes the queue in its paused state. Use the **Mixtape tray icon** (it might be under the notification area’s hidden-icons arrow): left-click to reopen; right-click for **Open Mixtape**, **Open music folder**, or **Quit Mixtape**. Windows can display a notification when the current batch finishes; this depends on notification settings. You can also reopen by double-clicking `Mixtape.exe` and quit from Settings. **Quit Mixtape** notifies all connected Mixtape tabs to attempt `window.close()` and then stops the tray and server. Firefox and other browsers can block script-closing a normal externally opened tab; in that case, the tab displays **Mixtape is closed** and can be closed manually. Other browser tabs are not affected. Finished album rows have **Open folder** buttons; completed song rows have **Show file** buttons, selecting the MP3 in Explorer. URL playlist jobs open the From Links folder. Newly queued albums expand to show track checkboxes: unchecking queues a **Skipped** state, checking again requeues the song; already saved songs cannot be unchecked because this never deletes files. Choose your download folder with **Settings → Browse…**, then click **Save settings**. If anything fails, switch to My queue: you can inspect **Technical details**, use **Copy error** to share a diagnostic report (review its paths/URLs before sharing), or press **Retry failed (N)**. Jobs needing manual selection stay in **Needs review**. Windows sleep is suppressed while the worker is processing a job when enabled in Settings. Logging out or turning off Windows stops the process; interrupted jobs are re-queued when reopened.

## Downloader updates

Mixtape uses the `--update` option provided by the official yt-dlp executable to check the **stable** release channel. It never runs this while songs are queued or downloading. A successful check is remembered for seven days; after a failed check, Mixtape tries again the next day. An update error leaves the app running and does not delete the existing helper. **Settings → Check now** triggers an immediate check while the queue is idle. This updates yt-dlp only; FFmpeg and Deno continue to use their existing binaries. This process requires Internet access and the extracted `tools` directory must be writable. For restricted/read-only folders, move the portable app to a location you can modify.

## Updating Mixtape from GitHub

Once an update-enabled Mixtape has been installed, **nothing needs to be downloaded or unzipped manually on your niece's PC**. A new stable GitHub release is detected automatically, and a single **Update & restart** click applies it while no songs are downloading. There is also **Settings → Mixtape updates → Check for updates**. Failed downloads, bad hashes, read-only installation folders, or a missing release ZIP cause an error rather than replacing a working EXE. The updater runs only for the public `https://github.com/f-stuff/Mixtape` repository. It relies on GitHub's release-asset SHA-256 digest and downloads over HTTPS.

For the maintainer, keep using either build script and GitHub Releases:

1. Make and test a new build. For clarity, increment `appVersion` in `main.go` (e.g. `1.4.1`) **before building**. This is recommended for fresh manual ZIP installs; auto-installed copies separately remember the verified release tag.
2. Run `build.bat` or `build-portable.bat` and upload **the resulting release ZIP** as an asset to a new GitHub release. The filenames must remain `Mixtape-Portable-Windows.zip` or `Mixtape-Complete-Portable-Windows.zip` (the latter takes precedence when both are present).
3. Give the release a new, increasing stable tag like `v1.4.1`, `v1.4.2` or `v1.5`. Make it **published**, not a draft or prerelease, and set it as Latest. Replacing the ZIP under the exact same tag will not reliably trigger the initial automatic update. These are patch tags within the v1.4 series, not a new major feature release.

The update helper uses the PowerShell already included with Windows 10/11; no installation or administrator rights are needed if Mixtape was unzipped into a writable folder. It creates a copy of existing files before replacing them and restores them if the file-copy stage fails. It changes only Mixtape application files and fills missing `tools` files; it never deletes downloaded MP3s or `%LOCALAPPDATA%/Mixtape/queue.json`. Update diagnostics, if necessary, are saved in `%LOCALAPPDATA%/Mixtape/last-app-update-error.txt`. If the GitHub repository becomes private, the unauthenticated update check cannot see its releases.

## Important limitations

- This v1.4.1 version has been **cross-compiled**; Go lifecycle/race tests and updater SHA-256/ZIP validation tests pass. The actual PowerShell EXE replacement/relaunch and Windows-native tray/browser integration still require testing on a real Windows PC. A live YouTube download was not retested for this change.
- Only YouTube video and playlist URLs are accepted in the URL download panel. Search by artist uses MusicBrainz metadata and *YouTube* `ytsearch` results, not an official YouTube Music catalogue endpoint. Public YouTube Music links are supported.
- Some videos are unavailable, blocked, require login, or change format. A result matching only superficially may be sent for manual review.
- In some album discographies, MusicBrainz may not know every release, and album artwork may be missing.
- Automatic helper setup and downloader update checks require access to GitHub and gyan.dev. Audio extraction also relies on the downloader's EJS challenge script support. Windows may suppress completion banners while Do Not Disturb is enabled.
- Browsers generally prohibit a website from closing ordinary tabs that weren't opened by JavaScript. Mixtape **cannot guarantee that Firefox will close its tab automatically**. It tries, then shows a clear closed message when blocked. Reconfiguring Firefox security settings or force-closing its process is deliberately not required or attempted.
- The executable is unsigned; Windows SmartScreen may warn about an unfamiliar program.
- This tool does not circumvent DRM, paywalls or access restrictions. Download only recordings you are authorised to save.

## Third-party components

Mixtape uses these public resources:

- [MusicBrainz API](https://musicbrainz.org/doc/MusicBrainz_API) — artists, release groups, tracklists. Be considerate of rate limits.
- [Cover Art Archive](https://musicbrainz.org/doc/Cover_Art_Archive/API) — album covers.
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) — YouTube search and extraction; `2026.10.26` x64 EXE (SHA-256 pinned for initial setup; subsequently updated to official stable releases).
- [FFmpeg](https://www.gyan.dev/ffmpeg/builds/) — static Windows MP3 conversion and ID3 tags; check applicable LGPL/GPL terms depending on the downloaded build.
- [Deno](https://deno.com/) — JS challenge runtime for yt-dlp; `v2.9.8` x64 ZIP (SHA-256 pinned).
- yt-dlp's [EJS support guide](https://github.com/yt-dlp/yt-dlp/wiki/EJS) — challenge solver loaded using `--remote-components ejs:github`.

See `THIRD_PARTY.md` for further licence/source information.
