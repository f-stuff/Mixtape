//go:build windows

package main

import (
	_ "embed"
	"os/exec"
	"strconv"
)

//go:embed scripts/apply-update.ps1
var releaseInstallerScript []byte

func startReleaseInstaller(script, installDir, stageDir, dataDir, tag string, processID int) error {
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
		"-File", script, "-InstallDir", installDir, "-StageDir", stageDir, "-DataDir", dataDir, "-ReleaseTag", tag, "-ParentPid", strconv.Itoa(processID))
	cmd.Dir = stageDir
	hideCommandWindow(cmd)
	return cmd.Start()
}
