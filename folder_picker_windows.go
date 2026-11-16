//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// BROWSEINFOW is the Win64 layout used by the Windows Shell's native
// folder-selection dialog. No PowerShell, browser permissions, or other
// tools are required, and the browser never sees the user's filesystem.
type browseInfo struct {
	owner       uintptr
	root        uintptr
	displayName *uint16
	title       *uint16
	flags       uint32
	callback    uintptr
	callbackArg uintptr
	image       int32
}

var (
	folderPickerMu         sync.Mutex
	folderBrowse           = shell32.NewProc("SHBrowseForFolderW")
	folderGetPath          = shell32.NewProc("SHGetPathFromIDListW")
	folderGetPathEx        = shell32.NewProc("SHGetPathFromIDListEx")
	folderFree             = ole32.NewProc("CoTaskMemFree")
	folderSendMessage      = user32.NewProc("SendMessageW")
	folderForegroundWindow = user32.NewProc("GetForegroundWindow")
	folderCallbackPtr      = syscall.NewCallback(folderPickerCallback)
)

func folderPickerCallback(hwnd uintptr, msg uint32, _ uintptr, initialPath uintptr) uintptr {
	const bffmInitialized = 1
	const bffmSetSelectionW = 0x0400 + 103
	if msg == bffmInitialized && initialPath != 0 {
		// Selecting the user's current folder makes the picker useful even
		// when their library lives on another drive.
		folderSendMessage.Call(hwnd, bffmSetSelectionW, 1, initialPath)
	}
	return 0
}

// chooseFolder returns (path, selected, error). Cancellation isn't an error.
func chooseFolder(initial string) (string, bool, error) {
	if !folderPickerMu.TryLock() {
		return "", false, fmt.Errorf("A folder picker is already open")
	}
	defer folderPickerMu.Unlock()

	// SHBrowseForFolder with BIF_NEWDIALOGSTYLE requires an STA. A Go
	// HTTP worker might run on any OS thread, so pin it while the dialog
	// is displayed and balance COM initialization on that same thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := coInitialize.Call(0, 2) // COINIT_APARTMENTTHREADED
	if hr != 0 && hr != 1 {
		return "", false, fmt.Errorf("Could not initialize the Windows folder picker (0x%x)", uint32(hr))
	}
	defer coUninitialize.Call()

	if st, err := os.Stat(initial); err != nil || !st.IsDir() || !filepath.IsAbs(initial) {
		initial, _ = os.UserHomeDir()
	}
	initialUTF16, err := syscall.UTF16PtrFromString(initial)
	if err != nil {
		return "", false, err
	}
	prompt, _ := syscall.UTF16PtrFromString("Choose where Mixtape saves music")
	var name [260]uint16
	owner, _, _ := folderForegroundWindow.Call()
	info := browseInfo{
		owner: owner, displayName: &name[0], title: prompt,
		flags:    0x0001 | 0x0010 | 0x0040, // RETURNONLYFSDIRS | EDITBOX | NEWDIALOGSTYLE
		callback: folderCallbackPtr, callbackArg: uintptr(unsafe.Pointer(initialUTF16)),
	}
	pidl, _, _ := folderBrowse.Call(uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(initialUTF16) // pointer also used by the dialog's callback
	if pidl == 0 {
		return "", false, nil // Cancel
	}
	defer folderFree.Call(pidl)

	// The Ex version supports long Windows paths on Windows 10/11.
	var path [32768]uint16
	valid, _, _ := folderGetPathEx.Call(pidl, uintptr(unsafe.Pointer(&path[0])), uintptr(len(path)), 0)
	if valid == 0 {
		var shortPath [260]uint16
		valid, _, _ = folderGetPath.Call(pidl, uintptr(unsafe.Pointer(&shortPath[0])))
		if valid == 0 {
			return "", false, fmt.Errorf("The selected location is not a filesystem folder")
		}
		return syscall.UTF16ToString(shortPath[:]), true, nil
	}
	return syscall.UTF16ToString(path[:]), true, nil
}
