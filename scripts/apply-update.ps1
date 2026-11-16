# Mixtape's temporary updater. Run only from Mixtape after a GitHub release ZIP
# has passed its SHA-256 check and safe-file extraction. No elevation required.
# Runs outside the installed executable so Windows can release Mixtape.exe.
param(
    [Parameter(Mandatory=$true)][string]$InstallDir,
    [Parameter(Mandatory=$true)][string]$StageDir,
    [Parameter(Mandatory=$true)][string]$DataDir,
    [Parameter(Mandatory=$true)][string]$ReleaseTag,
    [Parameter(Mandatory=$true)][int]$ParentPid
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$payload = Join-Path $StageDir 'payload'
$appExe = Join-Path $InstallDir 'Mixtape.exe'
$sourceExe = Join-Path $payload 'Mixtape.exe'
$backupDir = Join-Path $StageDir 'previous-files'
$errorFile = Join-Path $DataDir 'last-app-update-error.txt'
$replaced = New-Object 'System.Collections.Generic.List[object]'

function Remove-LargeStagingFiles {
    # The installer script is tiny, but a bundled ZIP and extracted tools can
    # be hundreds of megabytes. Do not leave them behind after applying.
    foreach ($path in @((Join-Path $StageDir 'release.zip'), $payload, $backupDir)) {
        try { if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Recurse -Force } }
        catch { }
    }
}

function Start-Mixtape {
    if (Test-Path -LiteralPath $appExe -PathType Leaf) {
        Start-Process -FilePath $appExe -WorkingDirectory $InstallDir | Out-Null
    }
}

try {
    if (-not (Test-Path -LiteralPath $sourceExe -PathType Leaf)) {
        throw 'The downloaded release did not include Mixtape.exe.'
    }
    if (-not (Test-Path -LiteralPath $appExe -PathType Leaf)) {
        throw 'Could not locate the installed Mixtape.exe.'
    }
    if ($ReleaseTag -notmatch '^[vV]?\d+\.\d+(\.\d+){0,2}$') {
        throw 'The release tag has an unexpected format.'
    }

    # Do not replace the running Windows executable. Allow 90 seconds for
    # the local server, tray and worker to shut down cleanly.
    $deadline = (Get-Date).AddSeconds(90)
    while ((Get-Process -Id $ParentPid -ErrorAction SilentlyContinue) -and (Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 250
    }
    if (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue) {
        throw 'Mixtape did not finish closing; the installed files were not changed.'
    }

    New-Item -ItemType Directory -Force -Path $backupDir | Out-Null
    $files = @(Get-ChildItem -LiteralPath $payload -File -Recurse |
        Sort-Object @{ Expression = { if ($_.Name -eq 'Mixtape.exe') { 1 } else { 0 } } })
    foreach ($file in $files) {
        $relative = $file.FullName.Substring($payload.Length + 1)
        $destination = Join-Path $InstallDir $relative
        # Keep already-installed media helpers: yt-dlp may have updated itself
        # after the release ZIP was built. Fill missing helper files only.
        if ($relative.StartsWith('tools\', [System.StringComparison]::OrdinalIgnoreCase) -and
            (Test-Path -LiteralPath $destination -PathType Leaf)) {
            continue
        }
        $previous = Join-Path $backupDir $relative
        $hadPrevious = Test-Path -LiteralPath $destination -PathType Leaf
        if ($hadPrevious) {
            New-Item -ItemType Directory -Path (Split-Path -Parent $previous) -Force | Out-Null
            Copy-Item -LiteralPath $destination -Destination $previous -Force
        }
        New-Item -ItemType Directory -Path (Split-Path -Parent $destination) -Force | Out-Null
        $replaced.Add(@{ Destination=$destination; Previous=$previous; HadPrevious=$hadPrevious })
        Copy-Item -LiteralPath $file.FullName -Destination $destination -Force
    }

    if (-not (Test-Path -LiteralPath $appExe -PathType Leaf)) {
        throw 'The new Mixtape.exe was not installed.'
    }
    # A successful update is remembered by its tag AND executable checksum.
    # If a user later replaces the EXE manually, the marker no longer matches.
    $exeHash = (Get-FileHash -LiteralPath $appExe -Algorithm SHA256).Hash.ToLowerInvariant()
    $marker = @{ tag=$ReleaseTag; exeSHA256=$exeHash } | ConvertTo-Json -Compress
    $markerPath = Join-Path $DataDir 'app-release.json'
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($markerPath, $marker, $utf8)
    if (Test-Path -LiteralPath $errorFile) { Remove-Item -LiteralPath $errorFile -Force }
    Start-Mixtape
    Remove-LargeStagingFiles
    # Do not remove the staging folder while this script is still being read.
    # A later update can safely remove old release-* directories if needed.
    exit 0
}
catch {
    $message = [string]$_.Exception.Message
    # Restore originals if ANY file replacement fails, including the EXE.
    for ($i = $replaced.Count - 1; $i -ge 0; $i--) {
        $entry = $replaced[$i]
        try {
            if ($entry.HadPrevious) {
                Copy-Item -LiteralPath $entry.Previous -Destination $entry.Destination -Force
            } elseif (Test-Path -LiteralPath $entry.Destination) {
                Remove-Item -LiteralPath $entry.Destination -Force
            }
        } catch { }
    }
    try {
        New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
        [System.IO.File]::WriteAllText($errorFile, $message)
        Start-Mixtape
    } catch { }
    Remove-LargeStagingFiles
    exit 1
}
