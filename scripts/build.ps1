# Builds Mixtape for Windows 10/11 x64 from the source tree.
# No administrator rights or installed Go toolchain are required.
# Examples:
#   .\scripts\build.ps1
#   .\scripts\build.ps1 -BundleTools
#   .\scripts\build.ps1 -TestsOnly

param(
    [switch]$BundleTools,
    [switch]$SkipTests,
    [switch]$TestsOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$root = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$cache = Join-Path $root '.build-cache'
$dist = Join-Path $root 'dist'

function Test-GoCompatible([string]$exe) {
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { return $false }
    try {
        $versionText = (& $exe version 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0) { return $false }
        if ($versionText -notmatch '\bgo(\d+)\.(\d+)') { return $false }
        $majorVersion = [int]$Matches[1]
        $minorVersion = [int]$Matches[2]
        return ($majorVersion -gt 1 -or ($majorVersion -eq 1 -and $minorVersion -ge 23))
    } catch {
        return $false
    }
}

function Get-VerifiedDownload([string]$url, [string]$target, [string]$hash = '') {
    $parent = Split-Path -Parent $target
    New-Item -ItemType Directory -Force -Path $parent | Out-Null
    $check = $hash.ToLowerInvariant()
    if (Test-Path -LiteralPath $target -PathType Leaf) {
        $size = (Get-Item -LiteralPath $target).Length
        if ($size -gt 1000 -and ($check -eq '' -or (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant() -eq $check)) {
            return
        }
        Remove-Item -LiteralPath $target -Force
    }
    Write-Host "Downloading $([System.IO.Path]::GetFileName($target))..."
    $partial = "$target.part"
    try {
        if (Test-Path -LiteralPath $partial) { Remove-Item -LiteralPath $partial -Force }
        Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $partial
        $size = (Get-Item -LiteralPath $partial).Length
        if ($size -lt 1000) { throw 'Downloaded file is unexpectedly small.' }
        if ($check -ne '') {
            $actual = (Get-FileHash -LiteralPath $partial -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actual -ne $check) { throw "SHA-256 mismatch for $target. Expected $check, received $actual" }
        }
        Move-Item -LiteralPath $partial -Destination $target -Force
    } finally {
        if (Test-Path -LiteralPath $partial) { Remove-Item -LiteralPath $partial -Force }
    }
}

function Find-Go {
    $installed = Get-Command go.exe -ErrorAction SilentlyContinue
    if ($null -ne $installed -and (Test-GoCompatible $installed.Source)) {
        Write-Host "Using installed Go: $($installed.Source)"
        return $installed.Source
    }
    $portable = Join-Path $cache 'go\go\bin\go.exe'
    if (Test-GoCompatible $portable) {
        Write-Host 'Using cached portable Go compiler.'
        return $portable
    }
    Write-Host 'Go 1.23+ not found. Fetching the official Windows compiler (one time, no installation)...'
    # The official Go downloads API supplies the file name and SHA-256 for the latest stable version.
    $releases = Invoke-RestMethod -Uri 'https://go.dev/dl/?mode=json'
    $release = $releases | Where-Object { $_.stable -eq $true } | Select-Object -First 1
    if ($null -eq $release) { throw 'Could not identify an official stable Go release.' }
    $asset = $release.files | Where-Object { $_.os -eq 'windows' -and $_.arch -eq 'amd64' -and $_.kind -eq 'archive' -and $_.filename -like '*.zip' } | Select-Object -First 1
    if ($null -eq $asset) { throw 'Could not find an official Windows x64 Go ZIP.' }
    $name = [string]$asset.filename
    $hash = [string]$asset.sha256
    if ($name -notmatch '^go\d+\.\d+(\.\d+)?\.windows-amd64\.zip$' -or $hash -notmatch '^[0-9a-fA-F]{64}$') {
        throw 'The Go download manifest has an unexpected format.'
    }
    $goCache = Join-Path $cache 'go'
    New-Item -ItemType Directory -Force -Path $goCache | Out-Null
    $archive = Join-Path $goCache $name
    Get-VerifiedDownload "https://go.dev/dl/$name" $archive $hash
    $old = Join-Path $goCache 'go'
    if (Test-Path -LiteralPath $old) { Remove-Item -LiteralPath $old -Recurse -Force }
    Write-Host 'Unpacking the Go compiler...'
    Expand-Archive -LiteralPath $archive -DestinationPath $goCache -Force
    if (-not (Test-GoCompatible $portable)) { throw 'Portable Go did not unpack correctly.' }
    return $portable
}

function Add-BundledTools([string]$staging) {
    $deps = Join-Path $cache 'dependencies'
    $tools = Join-Path $staging 'tools'
    New-Item -ItemType Directory -Force -Path $tools | Out-Null

    $ytdlp = Join-Path $deps 'yt-dlp.exe'
    Get-VerifiedDownload 'https://github.com/yt-dlp/yt-dlp/releases/download/2026.10.26/yt-dlp.exe' $ytdlp 'f5389d3698ecde4ef06f4e6b1ce0294fe9ac1396228c8990b8d3416d6d177a7d'
    Copy-Item -LiteralPath $ytdlp -Destination (Join-Path $tools 'yt-dlp.exe') -Force

    $denoZip = Join-Path $deps 'deno-x64.zip'
    Get-VerifiedDownload 'https://github.com/denoland/deno/releases/download/v2.9.8/deno-x86_64-pc-windows-msvc.zip' $denoZip 'cba84b26e0df5ae7f74fc66b86a95b866872a86292ebd84fa1e959927edd93bf'
    $denoTemp = Join-Path $cache 'deno-expanded'
    if (Test-Path -LiteralPath $denoTemp) { Remove-Item -LiteralPath $denoTemp -Recurse -Force }
    Expand-Archive -LiteralPath $denoZip -DestinationPath $denoTemp -Force
    $denoExe = Get-ChildItem -LiteralPath $denoTemp -Filter 'deno.exe' -Recurse -File | Select-Object -First 1
    if ($null -eq $denoExe) { throw 'Deno executable missing in archive.' }
    Copy-Item -LiteralPath $denoExe.FullName -Destination (Join-Path $tools 'deno.exe') -Force

    $ffmpegZip = Join-Path $deps 'ffmpeg-release-essentials.zip'
    # This vendor supplies a rolling archive rather than a versioned, hash-pinned release.
    Get-VerifiedDownload 'https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip' $ffmpegZip
    $ffmpegTemp = Join-Path $cache 'ffmpeg-expanded'
    if (Test-Path -LiteralPath $ffmpegTemp) { Remove-Item -LiteralPath $ffmpegTemp -Recurse -Force }
    Expand-Archive -LiteralPath $ffmpegZip -DestinationPath $ffmpegTemp -Force
    foreach ($exeName in @('ffmpeg.exe', 'ffprobe.exe')) {
        $bin = Get-ChildItem -LiteralPath $ffmpegTemp -Filter $exeName -Recurse -File | Select-Object -First 1
        if ($null -eq $bin) { throw "Missing $exeName in FFmpeg archive." }
        Copy-Item -LiteralPath $bin.FullName -Destination (Join-Path $tools $exeName) -Force
    }
    Write-Host 'Bundled tools: yt-dlp, Deno, FFmpeg, ffprobe.'
}

$previousLocation = Get-Location
$priorGoOs = $env:GOOS
$priorGoArch = $env:GOARCH
$priorCgo = $env:CGO_ENABLED
$priorPath = $env:PATH
try {
    Set-Location -LiteralPath $root
    $goExe = Find-Go
    $env:PATH = (Split-Path -Parent $goExe) + ';' + $env:PATH
    Write-Host (& $goExe version)
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'
    if (-not $SkipTests -or $TestsOnly) {
        Write-Host 'Running tests...'
        & $goExe test './...'
        if ($LASTEXITCODE -ne 0) { throw 'Go tests failed. ZIP was not produced.' }
    }
    if ($TestsOnly) {
        Write-Host 'All tests passed.'
    } else {
        New-Item -ItemType Directory -Path $dist -Force | Out-Null
        $stage = Join-Path $dist 'Mixtape-Portable'
        if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
        New-Item -ItemType Directory -Path $stage | Out-Null
        $exe = Join-Path $stage 'Mixtape.exe'
        Write-Host 'Compiling Mixtape.exe for Windows 10/11 x64...'
        & $goExe build -trimpath '-ldflags=-s -w -H windowsgui' -o $exe .
        if ($LASTEXITCODE -ne 0) { throw 'Go build failed. ZIP was not produced.' }
        if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { throw 'Build completed without Mixtape.exe.' }
        foreach ($docName in @('QUICK START.txt','README.md','THIRD_PARTY.md')) {
            Copy-Item -LiteralPath (Join-Path $root $docName) -Destination $stage -Force
        }
        if ($BundleTools) { Add-BundledTools $stage }
        $zipName = 'Mixtape-Portable-Windows.zip'
        if ($BundleTools) { $zipName = 'Mixtape-Complete-Portable-Windows.zip' }
        $zip = Join-Path $dist $zipName
        if (Test-Path -LiteralPath $zip) { Remove-Item -LiteralPath $zip -Force }
        Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -CompressionLevel Optimal
        Write-Host ''
        Write-Host 'Build successful!' -ForegroundColor Green
        Write-Host "Executable: $exe"
        Write-Host "Portable ZIP: $zip"
        if (-not $BundleTools) { Write-Host 'Note: On first launch the lightweight app fetches yt-dlp, FFmpeg, and Deno.' }
    }
} finally {
    Set-Location $previousLocation
    $env:GOOS = $priorGoOs
    $env:GOARCH = $priorGoArch
    $env:CGO_ENABLED = $priorCgo
    $env:PATH = $priorPath
}
