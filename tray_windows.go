//go:build windows

package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

//go:embed assets/mixtape.ico
var trayIconICO []byte

const (
	wmDestroy           = 0x0002
	wmClose             = 0x0010
	wmNull              = 0x0000
	wmContextMenu       = 0x007b
	wmLButtonUp         = 0x0202
	wmLButtonDblClk     = 0x0203
	wmRButtonUp         = 0x0205
	wmTray              = 0x8000 + 31 // WM_APP + 31
	wmShowNotice        = 0x8000 + 32 // Delivered on tray window's own UI thread
	ninBalloonUserClick = 0x0405      // Click on a Windows tray notification
	wmTimer             = 0x0113
	trayStatusTimer     = 1

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 1
	nifIcon    = 2
	nifTip     = 4
	nifInfo    = 0x10
	niifInfo   = 1

	menuOpen   = 101
	menuFolder = 102
	menuQuit   = 103
)

type windowsTray struct {
	app            *App
	hwnd           uintptr
	icon           uintptr
	iconOwned      bool
	added          bool
	taskbarCreated uint32
	lastOpen       time.Time
	lastStatus     string
	noticeWindow   atomic.Uintptr
	noticeMu       sync.Mutex
	pendingNotice  trayNotice
}

// A separate title lets the user distinguish background work from completion.
type trayNotice struct {
	title   string
	message string
}

// These definitions are the native Win64 API layouts; the build script targets
// windows/amd64 with CGO disabled (no systray runtime dependencies).
type trayClass struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	smallIcon  uintptr
}
type trayPoint struct{ x, y int32 }
type trayMsg struct {
	hwnd      uintptr
	message   uint32
	wParam    uintptr
	lParam    uintptr
	timestamp uint32
	pt        trayPoint
	private   uint32
}
type notifyIconData struct {
	size        uint32
	hwnd        uintptr
	id          uint32
	flags       uint32
	callback    uint32
	icon        uintptr
	tip         [128]uint16
	state       uint32
	stateMask   uint32
	info        [256]uint16
	version     uint32
	infoTitle   [64]uint16
	infoFlags   uint32
	guid        [16]byte
	balloonIcon uintptr
}

var (
	trayActive                atomic.Pointer[windowsTray]
	trayRegisterClass         = user32.NewProc("RegisterClassExW")
	trayCreateWindow          = user32.NewProc("CreateWindowExW")
	trayDefWindowProc         = user32.NewProc("DefWindowProcW")
	trayDestroyWindow         = user32.NewProc("DestroyWindow")
	trayGetMessage            = user32.NewProc("GetMessageW")
	trayTranslateMessage      = user32.NewProc("TranslateMessage")
	trayDispatchMessage       = user32.NewProc("DispatchMessageW")
	trayPostMessage           = user32.NewProc("PostMessageW")
	trayPostQuitMessage       = user32.NewProc("PostQuitMessage")
	traySetTimer              = user32.NewProc("SetTimer")
	trayKillTimer             = user32.NewProc("KillTimer")
	trayRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	trayGetCursorPos          = user32.NewProc("GetCursorPos")
	traySetForeground         = user32.NewProc("SetForegroundWindow")
	trayCreatePopup           = user32.NewProc("CreatePopupMenu")
	trayAppendMenu            = user32.NewProc("AppendMenuW")
	trayTrackPopup            = user32.NewProc("TrackPopupMenu")
	trayDestroyMenu           = user32.NewProc("DestroyMenu")
	trayLoadIcon              = user32.NewProc("LoadIconW")
	trayDestroyIcon           = user32.NewProc("DestroyIcon")
	trayCreateIcon            = user32.NewProc("CreateIconFromResourceEx")
	trayShellNotify           = shell32.NewProc("Shell_NotifyIconW")
	trayModuleHandle          = kernel32.NewProc("GetModuleHandleW")
	trayWndProcCallback       = syscall.NewCallback(trayWndProc)
)

// startTray creates a hidden native Win32 window to receive tray events. Its
// message loop stays on one OS thread; closing the browser never stops it.
// The returned function removes the icon when the application really quits.
func startTray(app *App) func() {
	t := &windowsTray{app: app}
	ready := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		if err := t.loop(ready); err != nil {
			log.Printf("Tray icon: %v", err)
		}
	}()
	err := <-ready
	if err != nil {
		return func() {}
	}
	hwnd := t.hwnd // Captured after ready: no data race with window teardown.
	var once sync.Once
	return func() {
		once.Do(func() {
			if hwnd != 0 {
				_, _, _ = trayPostMessage.Call(hwnd, wmClose, 0, 0)
			}
			select {
			case <-done:
			case <-time.After(850 * time.Millisecond):
				log.Print("Tray icon did not stop before shutdown")
			}
		})
	}
}

func (t *windowsTray) loop(ready chan<- error) error {
	if err := t.initialize(); err != nil {
		ready <- err
		t.cleanup()
		return err
	}
	ready <- nil
	var msg trayMsg
	for {
		result, _, err := trayGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if result == 0 {
			break
		}
		if result == ^uintptr(0) {
			t.cleanup()
			return fmt.Errorf("GetMessageW: %v", err)
		}
		trayTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		trayDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	t.cleanup()
	return nil
}

func (t *windowsTray) initialize() error {
	className, _ := syscall.UTF16PtrFromString("MixtapePortableTrayWindow")
	instance, _, _ := trayModuleHandle.Call(0)
	cls := trayClass{
		size:      uint32(unsafe.Sizeof(trayClass{})),
		wndProc:   trayWndProcCallback,
		instance:  instance,
		className: className,
	}
	if result, _, err := trayRegisterClass.Call(uintptr(unsafe.Pointer(&cls))); result == 0 {
		return fmt.Errorf("RegisterClassExW: %v", err)
	}
	t.icon, t.iconOwned = loadMixtapeIcon()
	if t.icon == 0 {
		return fmt.Errorf("could not create tray icon")
	}
	taskbarName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	created, _, _ := trayRegisterWindowMessage.Call(uintptr(unsafe.Pointer(taskbarName)))
	t.taskbarCreated = uint32(created)

	trayActive.Store(t)
	windowName, _ := syscall.UTF16PtrFromString("Mixtape tray")
	hwnd, _, err := trayCreateWindow.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(windowName)),
		0, 0, 0, 0, 0, 0, 0, instance, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW: %v", err)
	}
	t.hwnd = hwnd
	t.noticeWindow.Store(hwnd)
	if !t.addIcon() {
		return fmt.Errorf("Shell_NotifyIconW could not add tray icon")
	}
	// All native icon updates happen on this window's own UI thread.
	// Refresh only when the queue state changes, rather than leaving an
	// obsolete "running" tooltip after the final selected track finishes.
	if timer, _, _ := traySetTimer.Call(t.hwnd, trayStatusTimer, 1200, 0); timer == 0 {
		log.Print("Windows could not start the tray status timer")
	}
	return nil
}

func (t *windowsTray) notification() notifyIconData {
	n := notifyIconData{
		size: uint32(unsafe.Sizeof(notifyIconData{})),
		hwnd: t.hwnd, id: 1,
		flags:    nifMessage | nifIcon | nifTip,
		callback: wmTray, icon: t.icon,
	}
	copy(n.tip[:], syscall.StringToUTF16("Mixtape - "+t.app.queueStatus()))
	return n
}
func (t *windowsTray) addIcon() bool {
	n := t.notification()
	result, _, _ := trayShellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&n)))
	t.added = result != 0
	if t.added {
		t.lastStatus = t.app.queueStatus()
	}
	return t.added
}

// Refresh the tray tooltip independently of the browser's polling. The menu
// and the page use the same backend status so skipped tracks aren't treated
// as unfinished downloads by one surface but not the other.
func (t *windowsTray) refreshStatus() {
	if !t.added {
		return
	}
	status := t.app.queueStatus()
	if status == t.lastStatus {
		return
	}
	n := t.notification()
	n.flags = nifTip
	if ok, _, _ := trayShellNotify.Call(nimModify, uintptr(unsafe.Pointer(&n))); ok != 0 {
		t.lastStatus = status
	}
}

func (t *windowsTray) removeIcon() {
	if t.added {
		n := t.notification()
		trayShellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&n)))
		t.added = false
	}
}
func (t *windowsTray) cleanup() {
	if t.hwnd != 0 {
		trayKillTimer.Call(t.hwnd, trayStatusTimer)
	}
	t.noticeWindow.Store(0)
	t.removeIcon()
	if t.hwnd != 0 {
		trayDestroyWindow.Call(t.hwnd)
		t.hwnd = 0
	}
	if t.iconOwned && t.icon != 0 {
		trayDestroyIcon.Call(t.icon)
		t.icon = 0
	}
	trayActive.CompareAndSwap(t, nil)
}

func loadMixtapeIcon() (uintptr, bool) {
	// An ICO contains a 6-byte header and 16-byte directory entries. Windows'
	// CreateIconFromResourceEx can load the embedded image without an icon file
	// sitting next to Mixtape.exe.
	ico := trayIconICO
	if len(ico) >= 22 && binary.LittleEndian.Uint16(ico[2:4]) == 1 {
		count := int(binary.LittleEndian.Uint16(ico[4:6]))
		if len(ico) >= 6+count*16 && count > 0 {
			// Prefer the 32 px resource for the notification area.
			entry := 6
			for i := 0; i < count; i++ {
				at := 6 + 16*i
				if ico[at] == 32 {
					entry = at
					break
				}
			}
			length := int(binary.LittleEndian.Uint32(ico[entry+8 : entry+12]))
			offset := int(binary.LittleEndian.Uint32(ico[entry+12 : entry+16]))
			if length > 0 && offset >= 0 && offset <= len(ico) && length <= len(ico)-offset {
				resource := ico[offset : offset+length]
				h, _, _ := trayCreateIcon.Call(uintptr(unsafe.Pointer(&resource[0])), uintptr(length), 1, 0x00030000, 32, 32, 0)
				if h != 0 {
					return h, true
				}
			}
		}
	}
	// Generic Windows app icon if loading our art is unsupported on a host.
	h, _, _ := trayLoadIcon.Call(0, 32512) // IDI_APPLICATION
	return h, false
}

func trayWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	t := trayActive.Load()
	if t != nil && t.taskbarCreated != 0 && message == t.taskbarCreated {
		t.addIcon() // Restore after Explorer.exe restarts.
		return 0
	}
	switch message {
	case wmTimer:
		if t != nil && wParam == trayStatusTimer {
			t.refreshStatus()
		}
		return 0
	case wmShowNotice:
		if t != nil {
			t.showPendingNotice()
		}
		return 0
	case wmTray:
		if t != nil {
			switch uint32(lParam) {
			case wmLButtonUp, wmLButtonDblClk, ninBalloonUserClick:
				if time.Since(t.lastOpen) >= 750*time.Millisecond {
					t.lastOpen = time.Now()
					go openBrowser("http://" + listenAddress)
				}
			case wmRButtonUp, wmContextMenu:
				t.showMenu()
			}
		}
		return 0
	case wmClose:
		if t != nil {
			trayKillTimer.Call(hwnd, trayStatusTimer)
			t.removeIcon()
		}
		trayDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		if t != nil {
			t.removeIcon()
			t.hwnd = 0
		}
		trayPostQuitMessage.Call(0)
		return 0
	}
	result, _, _ := trayDefWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

// Runs on the tray's UI thread. Shell_NotifyIcon turns this legacy balloon
// request into a normal Windows 10/11 banner notification when permitted by
// the user's notification and Do Not Disturb settings.
func (t *windowsTray) showPendingNotice() {
	t.noticeMu.Lock()
	notice := t.pendingNotice
	t.pendingNotice = trayNotice{}
	t.noticeMu.Unlock()
	if notice.title == "" || notice.message == "" || !t.added {
		return
	}
	if notice.title == "Downloading in background" {
		// Do not show a delayed background notice once the job has finished.
		t.app.mu.Lock()
		stillPending := len(t.app.browserPages) == 0 && t.app.pendingDownloadsLocked()
		t.app.mu.Unlock()
		if !stillPending {
			return
		}
	}
	n := t.notification()
	n.flags = nifInfo
	n.infoFlags = niifInfo
	copy(n.info[:], syscall.StringToUTF16(notice.message))
	copy(n.infoTitle[:], syscall.StringToUTF16("Mixtape - "+notice.title))
	if ok, _, _ := trayShellNotify.Call(nimModify, uintptr(unsafe.Pointer(&n))); ok == 0 {
		log.Print("Windows did not accept the tray notification")
	}
}

func showTrayNotification(title, message string) {
	t := trayActive.Load()
	if t == nil || title == "" || message == "" {
		return
	}
	hwnd := t.noticeWindow.Load()
	if hwnd == 0 {
		return
	}
	t.noticeMu.Lock()
	t.pendingNotice = trayNotice{title: title, message: message}
	t.noticeMu.Unlock()
	trayPostMessage.Call(hwnd, wmShowNotice, 0, 0)
}

func (t *windowsTray) showMenu() {
	menu, _, _ := trayCreatePopup.Call()
	if menu == 0 {
		return
	}
	defer trayDestroyMenu.Call(menu)
	appendItem := func(id uintptr, label string) {
		text, _ := syscall.UTF16PtrFromString(label)
		trayAppendMenu.Call(menu, 0, id, uintptr(unsafe.Pointer(text)))
	}
	statusLabel, _ := syscall.UTF16PtrFromString(t.app.queueStatus())
	trayAppendMenu.Call(menu, 0x0001, 0, uintptr(unsafe.Pointer(statusLabel))) // MF_GRAYED
	trayAppendMenu.Call(menu, 0x0800, 0, 0)                                    // MF_SEPARATOR
	appendItem(menuOpen, "Open Mixtape")
	appendItem(menuFolder, "Open music folder")
	trayAppendMenu.Call(menu, 0x800, 0, 0) // MF_SEPARATOR
	appendItem(menuQuit, "Quit Mixtape")
	var point trayPoint
	if ok, _, _ := trayGetCursorPos.Call(uintptr(unsafe.Pointer(&point))); ok == 0 {
		return
	}
	traySetForeground.Call(t.hwnd)
	command, _, _ := trayTrackPopup.Call(menu, 0x0100|0x0080|0x0002,
		uintptr(point.x), uintptr(point.y), 0, t.hwnd, 0) // TPM_RETURNCMD | TPM_NONOTIFY | TPM_RIGHTBUTTON
	trayPostMessage.Call(t.hwnd, wmNull, 0, 0)
	switch command {
	case menuOpen:
		go openBrowser("http://" + listenAddress)
	case menuFolder:
		go func() {
			t.app.mu.Lock()
			folder := t.app.state.Settings.OutputDir
			t.app.mu.Unlock()
			if err := openFolder(folder); err != nil {
				showFatal("Could not open the music folder: " + err.Error())
			}
		}()
	case menuQuit:
		// Shutdown waits for this window's message loop from another goroutine.
		go func() {
			if t.app.shutdown != nil {
				t.app.shutdown()
			}
		}()
	}
}
