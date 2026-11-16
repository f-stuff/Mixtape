//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	user32         = syscall.NewLazyDLL("user32.dll")
	shell32        = syscall.NewLazyDLL("shell32.dll")
	setExecution   = kernel32.NewProc("SetThreadExecutionState")
	messageBox     = user32.NewProc("MessageBoxW")
	shellExecute   = shell32.NewProc("ShellExecuteW")
	ole32          = syscall.NewLazyDLL("ole32.dll")
	coInitialize   = ole32.NewProc("CoInitializeEx")
	coUninitialize = ole32.NewProc("CoUninitialize")
)

func hideCommandWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// ShellExecute uses the user's normal folder and browser associations and
// reports Shell errors. In contrast, starting explorer.exe can return success
// even when Explorer does not display the requested directory.
func openWithShell(verb, target, parameters string) error {
	// Folder shell extensions can require a COM single-threaded apartment.
	// Use an isolated locked thread so the Windows Shell behaves consistently
	// even when the request originates from Go's background HTTP worker.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := coInitialize.Call(0, 2) // COINIT_APARTMENTTHREADED
	if hr == 0 || hr == 1 {
		defer coUninitialize.Call()
	}
	action, err := syscall.UTF16PtrFromString(verb)
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	var args *uint16
	if parameters != "" {
		args, err = syscall.UTF16PtrFromString(parameters)
		if err != nil {
			return err
		}
	}
	result, _, callErr := shellExecute.Call(
		0, uintptr(unsafe.Pointer(action)), uintptr(unsafe.Pointer(file)),
		uintptr(unsafe.Pointer(args)), 0, 1, // SW_SHOWNORMAL
	)
	if result <= 32 {
		return fmt.Errorf("Windows could not open %q (shell code %d; %v)", target, result, callErr)
	}
	return nil
}

// Launch the URL with the Windows user's DEFAULT browser association.
// Do not force Edge, Chrome, or a separate browser profile.
func openBrowser(u string) {
	if err := openWithShell("open", u, ""); err != nil {
		log.Printf("Could not open default browser: %v", err)
	}
}

func openFolder(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("music folder is unavailable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("not a folder: %s", dir)
	}
	// Open the folder through Windows' registered Shell association. The
	// alternate "explore" verb covers machines with a modified association.
	if err := openWithShell("open", dir, ""); err != nil {
		if fallbackErr := openWithShell("explore", dir, ""); fallbackErr != nil {
			return fmt.Errorf("%v; Explorer fallback also failed: %v", err, fallbackErr)
		}
	}
	return nil
}

// Select the precise MP3 in Explorer rather than just opening its parent.
// For a directory (including an album), open the directory directly.
func openLocation(path string, selectFile bool) error {
	if !selectFile {
		return openFolder(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("downloaded file is unavailable: %w", err)
	}
	if info.IsDir() {
		return openFolder(path)
	}
	return openWithShell("open", "explorer.exe", `/select,"`+path+`"`)
}

func setKeepAwake(on bool) {
	var flag uintptr = 0x80000000 // ES_CONTINUOUS
	if on {
		flag |= 0x00000001
	} // ES_SYSTEM_REQUIRED
	_, _, _ = setExecution.Call(flag)
}
func showFatal(message string) {
	body, _ := syscall.UTF16PtrFromString(message)
	title, _ := syscall.UTF16PtrFromString("Mixtape")
	_, _, _ = messageBox.Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(title)), 0x10)
	log.Println(message)
}
