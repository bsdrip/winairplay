//go:build windows

package main

// The main window and the text input window.

import (
	"fmt"
	"log"
	"strings"
	"unsafe"
)

const (
	idcList       = 2001
	idcConnect    = 2002
	idcDisconnect = 2003
	idcSearch     = 2004
	idcForget     = 2005
	idcAddIP      = 2006
	idcSettings   = 2007

	mainStyle   = wsCaption | wsSysMenu | wsMinimizeBox
	mainClientW = 480
	mainClientH = 410
)

// mainWindow has the controls of the main window.
type mainWindow struct {
	status     uintptr
	listLabel  uintptr
	list       uintptr
	connect    uintptr
	disconnect uintptr
	search     uintptr
	forget     uintptr
	addIP      uintptr
	settings   uintptr
	rendered   string // the list contents that the list box shows now
}

func (a *app) createMainWindow() error {
	width, height := windowSizeForClient(a.px(mainClientW), a.px(mainClientH), mainStyle, 0)
	x := (systemMetric(smCxScreen) - width) / 2
	y := (systemMetric(smCyScreen) - height) / 3
	a.hwnd = createWindow(0, mainClassName, appName, mainStyle, x, y, width, height, 0, 0)
	if a.hwnd == 0 {
		return fmt.Errorf("create the main window")
	}
	w := &a.window
	child := func(class, text string, style uint32, x, y, width, height int32, id uintptr) uintptr {
		hwnd := createWindow(0, class, text, wsChild|wsVisible|style, a.px(x), a.px(y), a.px(width), a.px(height), a.hwnd, id)
		sendMessage(hwnd, wmSetFont, a.font, 1)
		return hwnd
	}
	const margin, full = 14, mainClientW - 28
	w.status = child("STATIC", a.status, ssLeft|ssNoPrefix, margin, 12, full, 36, 0)
	w.listLabel = child("STATIC", "Devices", ssLeft, margin, 54, full, 18, 0)
	w.list = child("LISTBOX", "", wsBorder|wsVScroll|wsTabStop|lbsNotify|lbsNoIntegralHeight, margin, 74, full, 216, idcList)
	w.connect = child("BUTTON", "Connect", wsTabStop|bsDefPushButton, margin, 300, 108, 30, idcConnect)
	w.disconnect = child("BUTTON", "Disconnect", wsTabStop, margin+114, 300, 108, 30, idcDisconnect)
	w.search = child("BUTTON", "Search again", wsTabStop, margin+228, 300, 108, 30, idcSearch)
	w.forget = child("BUTTON", "Forget", wsTabStop, margin+342, 300, 110, 30, idcForget)
	w.addIP = child("BUTTON", "Add by IP address...", wsTabStop, margin, 338, 165, 30, idcAddIP)
	w.settings = child("BUTTON", "Settings", wsTabStop, margin+171, 338, 108, 30, idcSettings)
	child("STATIC", appName+" "+version, ssRight|ssNoPrefix, margin+285, 345, full-285, 18, 0)
	child("STATIC", "If you close this window, winairplay continues in the notification area of the taskbar.",
		ssLeft|ssNoPrefix, margin, 378, full, 28, 0)
	return nil
}

func (a *app) showMainWindow() {
	a.refreshWindow()
	if iconic, _, _ := procIsIconic.Call(a.hwnd); iconic != 0 {
		showWindow(a.hwnd, swRestore)
	} else {
		showWindow(a.hwnd, swShow)
	}
	procSetForegroundWindow.Call(a.hwnd)
	procSetFocus.Call(a.window.list)
}

// selectedRow returns the index of the selected device, or -1.
func (a *app) selectedRow() int {
	index := int(int32(sendMessage(a.window.list, lbGetCurSel, 0, 0)))
	if index < 0 || index >= len(a.rows) {
		return -1
	}
	return index
}

// refreshWindow shows the current status and device list in the window.
func (a *app) refreshWindow() {
	w := &a.window
	if w.list == 0 {
		return
	}
	status := a.status
	if a.searching && a.session == nil {
		status += "\nSearching for AirPlay receivers..."
	} else if len(a.rows) == 0 {
		status += "\nNo AirPlay receiver found. Select \"Search again\" or \"Add by IP address...\"."
	}
	setWindowText(w.status, status)

	state := a.sessionState()
	labels := make([]string, len(a.rows))
	for i, row := range a.rows {
		labels[i] = rowLabel(row, state)
	}
	// Fill the list only when its contents changed, to keep the selection
	// stable and the picture without flicker.
	if rendered := strings.Join(labels, "\n"); rendered != w.rendered {
		selected := a.shownSelectionKey()
		sendMessage(w.list, lbResetContent, 0, 0)
		selection := -1
		for i, label := range labels {
			sendMessage(w.list, lbAddString, 0, uintptr(unsafe.Pointer(u16(label))))
			if selected != "" && deviceKey(a.rows[i].device) == selected {
				selection = i
			}
			if selection < 0 && a.rows[i].active {
				selection = i
			}
		}
		if selection < 0 && len(labels) > 0 {
			selection = 0
		}
		if selection >= 0 {
			sendMessage(w.list, lbSetCurSel, uintptr(selection), 0)
		}
		w.rendered = rendered
		a.renderedKeys = a.renderedKeys[:0]
		for _, row := range a.rows {
			a.renderedKeys = append(a.renderedKeys, deviceKey(row.device))
		}
	}
	a.updateButtons()
}

// shownSelectionKey returns the device key of the selection in the list
// that the window shows now (before a refill).
func (a *app) shownSelectionKey() string {
	index := int(int32(sendMessage(a.window.list, lbGetCurSel, 0, 0)))
	if index < 0 || index >= len(a.renderedKeys) {
		return ""
	}
	return a.renderedKeys[index]
}

func (a *app) updateButtons() {
	w := &a.window
	index := a.selectedRow()
	selected := index >= 0
	enableWindow(w.connect, selected && !a.rows[index].active)
	enableWindow(w.disconnect, a.session != nil)
	enableWindow(w.forget, selected && a.rows[index].known && !a.rows[index].active)
	enableWindow(w.search, !a.searching)
}

func (a *app) connectSelected() {
	if index := a.selectedRow(); index >= 0 && !a.rows[index].active {
		a.startSession(a.rows[index].device)
	}
}

func (a *app) showSettingsMenu() {
	menu := createPopupMenu()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	a.appendSettingsMenu(menu)
	var r rect
	procGetWindowRect.Call(a.window.settings, uintptr(unsafe.Pointer(&r)))
	a.trackMenu(menu, r.left, r.bottom)
}

func mainWndProc(hwnd, rawMessage, wParam, lParam uintptr) uintptr {
	a := theApp
	message := uint32(rawMessage)
	switch {
	case a == nil || a.hwnd != hwnd:
	case message == wmTray:
		switch lParam {
		case wmLButtonUp:
			a.showMainWindow()
		case wmRButtonUp:
			a.showTrayMenu()
		}
		return 0
	case message == wmUICall:
		a.runQueued()
		return 0
	case message == wmCommand:
		id, code := int(wParam&0xFFFF), int(wParam>>16&0xFFFF)
		switch {
		case id == idcList && code == lbnSelChange:
			a.updateButtons()
		case id == idcList && code == lbnDblClk:
			a.connectSelected()
		case id == idcConnect, id == idOK:
			a.connectSelected()
		case id == idcDisconnect:
			a.stopSession()
		case id == idcSearch:
			a.requestDiscovery()
		case id == idcForget:
			if index := a.selectedRow(); index >= 0 && a.rows[index].known && !a.rows[index].active {
				a.forgetDevice(a.rows[index].device)
			}
		case id == idcAddIP:
			a.mirrorToAddress()
		case id == idcSettings:
			a.showSettingsMenu()
		case id == idCancel:
			showWindow(hwnd, swHide)
		}
		return 0
	case message == wmClose:
		// The close button hides the window. "Exit" in the tray menu stops
		// the program.
		showWindow(hwnd, swHide)
		return 0
	case message == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case a.showUIMessage != 0 && message == a.showUIMessage:
		// A second start of winairplay (Start menu, Windows search).
		a.showMainWindow()
		return 0
	case a.taskbarCreated != 0 && message == a.taskbarCreated:
		// Explorer started again: the icon must be added again.
		a.addTrayIcon()
		return 0
	}
	return defWindowProc(hwnd, message, wParam, lParam)
}

// ---- text input window ----

const (
	promptClassName = "WinAirPlayPrompt"
	idEdit          = 100
)

type promptDialog struct {
	hwnd  uintptr
	edit  uintptr
	value string
	ok    bool
	done  bool
}

func (d *promptDialog) cancel() {
	if d.hwnd != 0 && !d.done {
		postMessage(d.hwnd, wmClose, 0, 0)
	}
}

func (d *promptDialog) finish(ok bool) {
	if d.done {
		return
	}
	if ok {
		d.value = windowText(d.edit)
	}
	d.ok = ok
	d.done = true
	procDestroyWindow.Call(d.hwnd)
}

func promptWndProc(hwnd, rawMessage, wParam, lParam uintptr) uintptr {
	a := theApp
	message := uint32(rawMessage)
	if a != nil && a.dialog != nil && a.dialog.hwnd == hwnd {
		switch message {
		case wmCommand:
			switch wParam & 0xFFFF {
			case idOK:
				a.dialog.finish(true)
				return 0
			case idCancel:
				a.dialog.finish(false)
				return 0
			}
		case wmClose:
			a.dialog.finish(false)
			return 0
		}
	}
	return defWindowProc(hwnd, message, wParam, lParam)
}

// prompt shows a small window with a text field and waits for the user. It
// runs a message loop of its own, so the tray icon continues to operate.
// Call it only on the UI thread.
func (a *app) prompt(title, text string) (string, bool) {
	if a.dialog != nil {
		return "", false
	}
	const clientW, clientH = 380, 150
	const style = wsPopup | wsCaption | wsSysMenu
	width, height := windowSizeForClient(a.px(clientW), a.px(clientH), style, wsExTopmost|wsExDlgFrame)
	x := (systemMetric(smCxScreen) - width) / 2
	y := (systemMetric(smCyScreen) - height) / 2
	d := &promptDialog{}
	d.hwnd = createWindow(wsExTopmost|wsExDlgFrame, promptClassName, title, style, x, y, width, height, 0, 0)
	if d.hwnd == 0 {
		log.Printf("could not create the input window")
		return "", false
	}
	a.dialog = d
	defer func() { a.dialog = nil }()

	child := func(class, label string, style uint32, x, y, width, height int32, id uintptr) uintptr {
		hwnd := createWindow(0, class, label, wsChild|wsVisible|style, a.px(x), a.px(y), a.px(width), a.px(height), d.hwnd, id)
		sendMessage(hwnd, wmSetFont, a.font, 1)
		return hwnd
	}
	child("STATIC", text, ssLeft|ssNoPrefix, 16, 14, clientW-32, 50, 0)
	d.edit = child("EDIT", "", wsBorder|wsTabStop|esAutoHScroll, 16, 70, clientW-32, 26, idEdit)
	child("BUTTON", "OK", wsTabStop|bsDefPushButton, clientW-208, 108, 92, 30, idOK)
	child("BUTTON", "Cancel", wsTabStop, clientW-108, 108, 92, 30, idCancel)

	showWindow(d.hwnd, swShow)
	procUpdateWindow.Call(d.hwnd)
	procSetForegroundWindow.Call(d.hwnd)
	procSetFocus.Call(d.edit)

	var m msg
	for !d.done {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			// WM_QUIT: give it back to the main loop.
			procPostQuitMessage.Call(m.wParam)
			if !d.done {
				d.done = true
				procDestroyWindow.Call(d.hwnd)
			}
			break
		}
		// IsDialogMessage gives the Tab, Enter and Escape keys their usual
		// dialog functions.
		if handled, _, _ := procIsDialogMessageW.Call(d.hwnd, uintptr(unsafe.Pointer(&m))); handled != 0 {
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	return d.value, d.ok
}
