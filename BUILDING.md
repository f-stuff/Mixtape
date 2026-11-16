# Build and modify Mixtape

**Windows 10/11, 64-bit.** You can edit the source with any editor, then create an updated `Mixtape.exe` without installing a development environment. No administrator permission is needed.

## One-click local builds

| Double-click | Result |
| --- | --- |
| **`build.bat`** | Runs tests, compiles `Mixtape.exe`, makes a lightweight portable ZIP. |
| **`build-portable.bat`** | Same, but downloads and includes the Windows yt-dlp, FFmpeg and Deno binaries in the ZIP, making first-run helper setup unnecessary. This ZIP is much larger. |
| **`test.bat`** | Runs automated tests without creating an EXE. |

The scripts use an existing **Go 1.23+** installation if available. Otherwise, they automatically download a **portable Go compiler** from the official go.dev release site into `.build-cache` (one time). The download is SHA-256-checked against the official release manifest. It is not installed system-wide. An Internet connection is needed for this first build; subsequent lightweight builds can be offline.

After `build.bat`, look in:

```
dist/
  Mixtape-Portable-Windows.zip
  Mixtape-Portable/
    Mixtape.exe
    QUICK START.txt
    README.md
    THIRD_PARTY.md
```

The default ZIP's executable will automatically download its media helpers on first launch (roughly 170 MB, depending on upstream versions). `build-portable.bat` instead produces `dist/Mixtape-Complete-Portable-Windows.zip`, containing a `tools/` folder next to `Mixtape.exe`. Keep that folder with the EXE when moving the bundled version.

## Editing the app

- `web/index.html` — dashboard layout and text
- `web/style.css` — design, colours and responsive layout
- `web/app.js` — interface behaviour, API calls, and per-tab shutdown beacons
- `queue_status.go` — one shared queue status for browser and Windows tray
- `main.go` — localhost web server, settings, persistent queue, per-job open-location endpoint, shutdown event stream, and version number
- `main.go` also stores skipped/reenabled song selections and runs the dedicated 1-second lifecycle check, independent from the downloader
- `browser_presence.go` — Firefox-friendly browser tab leases (30-second fallback), close notifications, stale-poll protection, and background-finish handling (beacon plus keepalive fetch)
- `platform_windows.go` — default-browser and Explorer folder/MP3 selection via Windows ShellExecute
- `tray_windows.go` — native Windows tray icon/menu (no Go dependencies); `assets/mixtape.ico` is embedded directly into the EXE
- `tray_windows.go` refreshes its native tooltip/menu status from the shared backend queue status, shows background-download and Done notifications, and opens the dashboard when a notification is clicked
- `folder_picker_windows.go` — native Win32 folder-selection dialog, called by Settings → Browse
- `musicbrainz.go` — artist, album and track metadata, with bounded retries for temporary API failures
- `matcher.go` — recording matching and review thresholds
- `worker.go` — search, queue, downloads, conversion and tagging
- `tools.go` — helper-tool setup and pinned initial downloads
- `updater.go` — automatic idle-only yt-dlp update checks, every seven days (manual option in Settings)
- `github_updates.go` — GitHub Releases app updates, safe ZIP verification/extraction, idle-queue protection, post-install version tracking
- `release_installer_windows.go` and `scripts/apply-update.ps1` — launch the separate Windows update helper; copy over portable app files after Mixtape quits and reopen it
- `batch.go` — completion summary and retry-all-failed logic
- `errors.go` — friendly failure messages and technical diagnostic text for the Copy error button
- `main_test.go`, `features_test.go`, `final_features_test.go`, `finishing14_test.go`, `close_flow_test.go` — tests

Go **embeds `web/` automatically**, so changes to HTML, CSS or JavaScript are picked up the next time you build; no Node.js, package manager or front-end build step is required. To change the displayed app version, edit `const appVersion` in `main.go`.

## Build from PowerShell or CI

From the repository root:

```powershell
.\scripts\build.ps1                 # Test + lightweight ZIP
.\scripts\build.ps1 -BundleTools    # Test + fully bundled ZIP
.\scripts\build.ps1 -TestsOnly      # Tests only
.\scripts\build.ps1 -SkipTests      # Fast iteration (not recommended for release)
```

PowerShell script policy may block scripts launched directly; the `.bat` entry points already use `-ExecutionPolicy Bypass` **for that PowerShell process only**. On GitHub, `.github/workflows/full-portable.yml` uses the same build script to publish a fully bundled ZIP as a workflow artifact. Windows and other machines can also build directly with Go as described in `README.md`.

## Changes and data

Build output and cached compilers are excluded by `.gitignore`; you can commit the actual source files without accidentally uploading executables. Rebuilding or replacing `Mixtape.exe` does **not** erase the existing collection/queue; they are stored under `%LOCALAPPDATA%\Mixtape` and in the music output folder. Quit an older running Mixtape instance from its tray menu or Settings before testing a new executable, or the new launch will reconnect to the old version. Downloaded MP3s and queued jobs survive rebuilding.

## Publishing updates through GitHub Releases

The built ZIP filenames remain stable for automatic updates: `Mixtape-Portable-Windows.zip` (from `build.bat`) or `Mixtape-Complete-Portable-Windows.zip` (from `build-portable.bat`). Upload either as a release asset to **a new published, stable GitHub release** at `https://github.com/f-stuff/Mixtape/releases`, with a greater version tag such as `v1.4.1` for this build (then `v1.4.2` for later changes). If both assets are attached, the updater prefers the complete ZIP. The repository must be public for clients to check without GitHub authentication.

To keep the displayed version correct for users who download the ZIP themselves, update `const appVersion` in `main.go` to the release tag's version *before* running the build script. Copies installed through the updater also remember the installed release tag so forgetting to bump that constant does not cause repeated update prompts. Replacing an asset within the **same** GitHub release tag is not a reliable way to notify existing installs; use a new increasing patch tag.

Mixtape checks for app updates on startup, then daily, without downloading any ZIP until the user clicks **Update & restart**. It verifies the release asset digest from GitHub, extracts only the supported app files, stops its own process, and then uses the embedded PowerShell script to replace those files and relaunch. Windows' normal PowerShell is required (no extra installation), and the extracted destination must be writable by the user. The script retains existing `tools` binaries to avoid downgrading separately updated yt-dlp, and it does not touch downloaded music or the JSON queue. **Windows-native update execution needs a real desktop test** before the public release is announced as fully verified.

## Release caveats

- Windows executable builds and Go unit tests can be verified on other hosts, but opening the default Windows browser and the native folder picker must also be **tested on Windows** before distributing a release.
- The downloaded Go ZIP and two pinned media helper binaries are checked against expected SHA-256 values. The FFmpeg vendor's `ffmpeg-release-essentials.zip` is a rolling archive and does not have a pinned hash in this script. Review upstream distribution and licensing before wider redistribution.
- The app is unsigned, so Windows may warn when first launching it. Do not dismiss warnings for untrusted files.
- Music search and media downloads require network access, and users should download only recordings they are authorised to save.
