//go:build !windows

package main

import (
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
)

func hideCommandWindow(cmd *exec.Cmd) {}
func openBrowser(u string)            { _ = exec.Command("xdg-open", u).Start() }
func openFolder(dir string) error     { return exec.Command("xdg-open", dir).Start() }
func openLocation(path string, selectFile bool) error {
	if selectFile {
		return openFolder(filepath.Dir(path))
	}
	return openFolder(path)
}
func setKeepAwake(on bool)     {}
func showFatal(message string) { log.Print(message) }

func chooseFolder(initial string) (string, bool, error) {
	return "", false, fmt.Errorf("The folder picker requires Windows")
}
