//go:build windows

package main

// Minimal Win32 bindings for the tray application. They use only
// golang.org/x/sys/windows, so the program needs no cgo and no GUI library.

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procUpdateWindow             = user32.NewProc("UpdateWindow")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procSendMessageW             = user32.NewProc("SendMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procIsDialogMessageW         = user32.NewProc("IsDialogMessageW")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procSetFocus                 = user32.NewProc("SetFocus")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procLoadIconW                = user32.NewProc("LoadIconW")
	procLoadCursorW              = user32.NewProc("LoadCursorW")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procMessageBoxW              = user32.NewProc("MessageBoxW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procGetStockObject           = gdi32.NewProc("GetStockObject")
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmSetFont   = 0x0030
	wmCommand   = 0x0111
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmApp       = 0x8000

	wsPopup      = 0x80000000
	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000
	wsChild      = 0x40000000
	wsVisible    = 0x10000000
	wsTabStop    = 0x00010000
	wsBorder     = 0x00800000
	wsExTopmost  = 0x00000008
	wsExDlgFrame = 0x00000001

	esAutoHScroll   = 0x0080
	bsDefPushButton = 0x0001
	ssLeft          = 0x0000

	swShow = 5

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo  = 0x01
	niifError = 0x03

	idiApplication = 32512
	idcArrow       = 32512
	colorBtnFace   = 15
	defaultGUIFont = 17

	smCxScreen  = 0
	smCyScreen  = 1
	smCMonitors = 80

	idOK     = 1
	idCancel = 2

	mbIconError = 0x10
	mbIconInfo  = 0x40
)

type point struct{ x, y int32 }

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

type wndClassEx struct {
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
	iconSm     uintptr
}

// notifyIconData is NOTIFYICONDATAW (Windows Vista and later layout).
type notifyIconData struct {
	size            uint32
	hwnd            uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32
	infoTitle       [64]uint16
	infoFlags       uint32
	guidItem        windows.GUID
	balloonIcon     uintptr
}

func u16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		p, _ = windows.UTF16PtrFromString("?")
	}
	return p
}

// copyU16 copies s into a fixed UTF-16 buffer and always leaves a final zero.
func copyU16(dst []uint16, s string) {
	src, err := windows.UTF16FromString(s)
	if err != nil {
		src = []uint16{'?', 0}
	}
	if len(src) > len(dst) {
		src = src[:len(dst)]
	}
	n := copy(dst, src)
	if n == len(dst) {
		n--
	}
	dst[n] = 0
}

func moduleHandle() uintptr {
	h, _, _ := procGetModuleHandleW.Call(0)
	return h
}

func registerClass(name string, proc uintptr, icon uintptr) error {
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassEx{
		wndProc:    proc,
		instance:   moduleHandle(),
		icon:       icon,
		iconSm:     icon,
		cursor:     cursor,
		background: colorBtnFace + 1,
		className:  u16(name),
	}
	wc.size = uint32(unsafe.Sizeof(wc))
	if atom, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		return err
	}
	return nil
}

func createWindow(exStyle uint32, class, title string, style uint32, x, y, w, h int32, parent, id uintptr) uintptr {
	hwnd, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(u16(class))),
		uintptr(unsafe.Pointer(u16(title))),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, id, moduleHandle(), 0)
	return hwnd
}

func defWindowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func postMessage(hwnd uintptr, message uint32, wParam, lParam uintptr) {
	procPostMessageW.Call(hwnd, uintptr(message), wParam, lParam)
}

func systemMetric(index int) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(r)
}

func windowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf)
}

func messageBox(title, text string, flags uint32) {
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(u16(text))), uintptr(unsafe.Pointer(u16(title))), uintptr(flags))
}

func appendMenu(menu uintptr, flags uint32, id uintptr, text string) {
	var p uintptr
	if flags&mfSeparator == 0 {
		p = uintptr(unsafe.Pointer(u16(text)))
	}
	procAppendMenuW.Call(menu, uintptr(flags), id, p)
}

func createPopupMenu() uintptr {
	h, _, _ := procCreatePopupMenu.Call()
	return h
}

// iconFromPNG makes an icon from PNG data. It returns 0 on failure.
func iconFromPNG(data []byte, size int) uintptr {
	if len(data) == 0 {
		return 0
	}
	h, _, _ := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)),
		1,          // fIcon
		0x00030000, // icon format version
		uintptr(size), uintptr(size), 0)
	return h
}

func stockApplicationIcon() uintptr {
	h, _, _ := procLoadIconW.Call(0, idiApplication)
	return h
}

// ---- additions for the main window ----

var (
	procFindWindowW              = user32.NewProc("FindWindowW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procEnableWindow             = user32.NewProc("EnableWindow")
	procSetWindowTextW           = user32.NewProc("SetWindowTextW")
	procAdjustWindowRectEx       = user32.NewProc("AdjustWindowRectEx")
	procSetProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procSystemParametersInfoW    = user32.NewProc("SystemParametersInfoW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procGetDeviceCaps            = gdi32.NewProc("GetDeviceCaps")
	procCreateFontIndirectW      = gdi32.NewProc("CreateFontIndirectW")
)

const (
	wsMinimizeBox = 0x00020000
	wsVScroll     = 0x00200000
	wsGroup       = 0x00020000

	lbsNotify           = 0x0001
	lbsNoIntegralHeight = 0x0100
	lbAddString         = 0x0180
	lbResetContent      = 0x0184
	lbSetCurSel         = 0x0186
	lbGetCurSel         = 0x0188
	lbnSelChange        = 1
	lbnDblClk           = 2

	ssRight    = 0x0002
	ssNoPrefix = 0x0080

	swHide    = 0
	swRestore = 9

	logPixelsX              = 88
	spiGetNonClientMetrics  = 0x0029
	nonClientMetricsSize    = 504
	nonClientMessageFontPos = 408
)

type rect struct{ left, top, right, bottom int32 }

func sendMessage(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func setWindowText(hwnd uintptr, text string) {
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(u16(text))))
}

func enableWindow(hwnd uintptr, enabled bool) {
	var flag uintptr
	if enabled {
		flag = 1
	}
	procEnableWindow.Call(hwnd, flag)
}

func showWindow(hwnd uintptr, command int) {
	procShowWindow.Call(hwnd, uintptr(command))
}

// systemDPI returns the dots per inch of the primary display (96 = 100 %).
func systemDPI() int32 {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return 96
	}
	defer procReleaseDC.Call(0, dc)
	dpi, _, _ := procGetDeviceCaps.Call(dc, logPixelsX)
	if dpi < 96 || dpi > 960 {
		return 96
	}
	return int32(dpi)
}

// messageFont returns the font that Windows uses for dialog text.
func messageFont() uintptr {
	var metrics [nonClientMetricsSize]byte
	*(*uint32)(unsafe.Pointer(&metrics[0])) = nonClientMetricsSize
	ok, _, _ := procSystemParametersInfoW.Call(spiGetNonClientMetrics, nonClientMetricsSize, uintptr(unsafe.Pointer(&metrics[0])), 0)
	if ok != 0 {
		if font, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&metrics[nonClientMessageFontPos]))); font != 0 {
			return font
		}
	}
	font, _, _ := procGetStockObject.Call(defaultGUIFont)
	return font
}

// windowSizeForClient returns the outer window size for a client area size.
func windowSizeForClient(width, height int32, style, exStyle uint32) (int32, int32) {
	r := rect{0, 0, width, height}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), uintptr(style), 0, uintptr(exStyle))
	return r.right - r.left, r.bottom - r.top
}
