//go:build !windows

package main

import (
	_ "embed"
	"errors"
)

// Keep the script embedded on non-Windows too so a release can be staged and
// verified in cross-platform tests; launching it requires Windows.
//
//go:embed scripts/apply-update.ps1
var releaseInstallerScript []byte

func startReleaseInstaller(script, installDir, stageDir, dataDir, tag string, processID int) error {
	return errors.New("automatic installation requires Windows 10/11")
}
