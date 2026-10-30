package term

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The clipboard is one program's at a time: another may hold it for a
// moment, so opening it is tried clipboardTries times, clipboardWait apart.
const (
	clipboardTries = 5
	clipboardWait  = 20 * time.Millisecond
	cfUnicodeText  = 13 // CF_UNICODETEXT
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	openClipboard    = user32.NewProc("OpenClipboard")
	closeClipboard   = user32.NewProc("CloseClipboard")
	getClipboardData = user32.NewProc("GetClipboardData")
	globalLock       = kernel32.NewProc("GlobalLock")
	globalUnlock     = kernel32.NewProc("GlobalUnlock")
)

// Clipboard is the text on the system clipboard, with Windows line ends made
// \n; none when it holds no text. A console window pastes by typing the
// text, so the TUI reads it here on Ctrl+V instead.
func Clipboard() (string, error) {
	var ok uintptr
	var err error
	for range clipboardTries {
		if ok, _, err = openClipboard.Call(0); ok != 0 {
			break
		}
		time.Sleep(clipboardWait)
	}
	if ok == 0 {
		return "", fmt.Errorf("the clipboard is in use: %w", err)
	}
	defer closeClipboard.Call()
	h, _, _ := getClipboardData.Call(cfUnicodeText)
	if h == 0 {
		return "", nil
	}
	p, _, err := globalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("the clipboard: %w", err)
	}
	defer globalUnlock.Call(h)
	s := windows.UTF16PtrToString(*(**uint16)(unsafe.Pointer(&p)))
	return strings.ReplaceAll(s, "\r\n", "\n"), nil
}
