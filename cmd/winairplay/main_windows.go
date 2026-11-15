//go:build windows

// winairplay mirrors the screen and the sound of a Windows PC to an Apple TV
// or another AirPlay receiver. It has a main window and an icon in the
// notification area of the taskbar.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/bsdrip/winairplay/internal/airplay"
	"github.com/bsdrip/winairplay/internal/mirror"
)

// version is set by the build (-ldflags "-X main.version=1.2.3").
var version = "dev"

const (
	appName       = "winairplay"
	mainClassName = "WinAirPlayMain"

	wmTray   = wmApp + 1 // message from the tray icon
	wmUICall = wmApp + 2 // run queued functions on the UI thread

	trayIconID = 1

	// Menu commands. Numbers 1 and 2 are IDOK and IDCANCEL in the window.
	idStop      = 11
	idRefresh   = 12
	idManualIP  = 13
	idOpenLog   = 14
	idExit      = 15
	idCursor    = 16
	idDebugLog  = 18
	idPicture4K = 19
	idPictureHD = 20
	idOpenUI    = 21
	idEncoder   = 100  // + index in encoderChoices
	idFPS       = 200  // + index in fpsChoices
	idDisplay   = 300  // + zero-based display index
	idSound     = 400  // + airplay.AudioOutputMode
	idDevice    = 1000 // + index in the device rows
	maxDevices  = 200
	maxDisplays = 16
)

var encoderChoices = []struct{ value, label string }{
	{"auto", "Automatic"},
	{"nvenc", "NVIDIA (NVENC)"},
	{"qsv", "Intel (Quick Sync)"},
	{"amf", "AMD (AMF)"},
	{"mf", "Windows Media Foundation"},
}

var fpsChoices = []int{15, 30, 60}

// soundLabels has one menu text for each airplay.AudioOutputMode.
var soundLabels = []string{
	"TV and this PC",
	"TV only (mutes this PC)",
	"This PC only (headphones or speakers)",
}

// knownDevice is a receiver that mirroring operated with before.
type knownDevice struct {
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	DeviceID string `json:"deviceID,omitempty"`
	LastUsed string `json:"lastUsed,omitempty"` // RFC 3339
}

func (k knownDevice) device() airplay.AirPlayDevice {
	return airplay.AirPlayDevice{Name: k.Name, Model: k.Model, IP: k.IP, Port: k.Port, DeviceID: k.DeviceID}
}

// settings are saved in %AppData%\winairplay\settings.json.
type settings struct {
	Encoder    string `json:"encoder"`
	FPS        int    `json:"fps"`
	Display    int    `json:"display"`
	HideCursor bool   `json:"hideCursor"`
	// Picture1080p selects H.264 at 1080p. The default is 4K (HEVC).
	Picture1080p bool `json:"picture1080p"`
	// SoundMode: 0 = TV and PC, 1 = TV only (mutes the PC), 2 = PC only.
	SoundMode int `json:"soundMode"`
	// NoSound is the setting of version 0.2. It is read as SoundMode 2.
	NoSound  bool          `json:"noSound,omitempty"`
	DebugLog bool          `json:"debugLog"`
	Known    []knownDevice `json:"knownDevices,omitempty"`
}

func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, appName, "settings.json")
}

func loadSettings() settings {
	s := settings{Encoder: "auto", FPS: 30}
	if data, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(data, &s)
	}
	if airplay.ValidateHWAccel(s.Encoder) != nil || s.Encoder == "" {
		s.Encoder = "auto"
	}
	if s.FPS <= 0 || s.FPS > 60 {
		s.FPS = 30
	}
	if s.Display < 0 || s.Display >= maxDisplays {
		s.Display = 0
	}
	if s.NoSound {
		s.NoSound = false
		s.SoundMode = int(airplay.AudioOutputPCOnly)
	}
	if s.SoundMode < 0 || s.SoundMode > int(airplay.AudioOutputPCOnly) {
		s.SoundMode = 0
	}
	known := s.Known[:0]
	for _, k := range s.Known {
		if k.IP != "" && k.Port > 0 && k.Port < 65536 {
			known = append(known, k)
		}
	}
	s.Known = known
	return s
}

func (s settings) save() {
	path := settingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("save settings: %v", err)
		return
	}
	data, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("save settings: %v", err)
	}
}

// activeSession is one running mirror session.
type activeSession struct {
	device airplay.AirPlayDevice
	cancel context.CancelFunc
	done   chan struct{}
}

// deviceRow is one line of the device list.
type deviceRow struct {
	device airplay.AirPlayDevice
	found  bool // the last searches found it
	known  bool // it is in the saved list
	active bool // the session uses it
}

type app struct {
	hwnd           uintptr
	iconIdle       uintptr
	iconActive     uintptr
	taskbarCreated uint32
	showUIMessage  uint32
	logPath        string
	dpi            int32
	font           uintptr
	window         mainWindow

	// The fields below are used only on the UI thread.
	settings settings
	devices  []airplay.AirPlayDevice // found by the search
	missed   map[string]int          // searches in a row that did not find a device
	rows     []deviceRow
	// renderedKeys has the device key of each line that the list box shows.
	renderedKeys []string
	searching    bool
	session      *activeSession
	streaming    bool
	status       string
	dialog       *promptDialog

	queueMu sync.Mutex
	queue   []func()

	refresh chan struct{}
}

var theApp *app

// px converts a length at 100 % display scale to pixels.
func (a *app) px(v int32) int32 { return v * a.dpi / 96 }

func main() {
	// Win32 windows belong to the thread that made them.
	runtime.LockOSThread()

	background := false
	for _, arg := range os.Args[1:] {
		if arg == "--background" || arg == "-background" {
			background = true
		}
	}

	showUIMessage := registerWindowMessage("WinAirPlayShowUI")
	mutexName, _ := windows.UTF16PtrFromString("Local\\winairplay-single-instance")
	if _, err := windows.CreateMutex(nil, false, mutexName); errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// winairplay runs already: ask that instance to show its window.
		if !background {
			activateRunningInstance(showUIMessage)
		}
		return
	}

	// Without this call Windows scales the window as a bitmap, and the text
	// is not sharp on a 4K monitor.
	procSetProcessDPIAware.Call()

	a := &app{settings: loadSettings(), status: "Not connected", refresh: make(chan struct{}, 1)}
	theApp = a
	a.showUIMessage = showUIMessage
	a.dpi = systemDPI()
	a.font = messageFont()
	a.logPath = setupLog()
	airplay.SetDebugMode(a.settings.DebugLog)
	airplay.SetAudioOutputMode(airplay.AudioOutputMode(a.settings.SoundMode))
	log.Printf("%s %s started", appName, version)

	if err := a.createWindows(); err != nil {
		log.Printf("startup failed: %v", err)
		messageBox(appName, "winairplay could not start: "+err.Error(), mbIconError)
		return
	}
	a.addTrayIcon()
	a.rebuildRows()
	if !background {
		a.showMainWindow()
	}
	go a.discoveryLoop()

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		// IsDialogMessage gives the Tab and Enter keys their usual functions
		// in the main window.
		if handled, _, _ := procIsDialogMessageW.Call(a.hwnd, uintptr(unsafe.Pointer(&m))); handled != 0 {
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	log.Printf("%s stopped", appName)
}

func registerWindowMessage(name string) uint32 {
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16(name))))
	return uint32(r)
}

// activateRunningInstance tells the first instance to show its window.
func activateRunningInstance(message uint32) {
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(u16(mainClassName))), 0)
	if hwnd == 0 || message == 0 {
		messageBox(appName, "winairplay is already running. Look for its icon in the notification area of the taskbar.", mbIconInfo)
		return
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	// Windows lets a program come to the foreground only with permission
	// from the program that has the foreground now.
	procAllowSetForegroundWindow.Call(uintptr(pid))
	postMessage(hwnd, message, 0, 0)
}

// setupLog sends the log to %LocalAppData%\winairplay\winairplay.log. A GUI
// program has no console.
func setupLog() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, appName)
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, appName+".log")
	if info, err := os.Stat(path); err == nil && info.Size() > 2<<20 {
		_ = os.Rename(path, path+".old")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.SetOutput(io.Discard)
		return ""
	}
	log.SetOutput(file)
	return path
}

func (a *app) createWindows() error {
	a.iconIdle = iconFromPNG(drawIcon(32, false), 32)
	a.iconActive = iconFromPNG(drawIcon(32, true), 32)
	if a.iconIdle == 0 {
		a.iconIdle = stockApplicationIcon()
	}
	if a.iconActive == 0 {
		a.iconActive = a.iconIdle
	}
	windowIcon := a.iconActive
	if err := registerClass(mainClassName, windows.NewCallback(mainWndProc), windowIcon); err != nil {
		return fmt.Errorf("register window class: %w", err)
	}
	if err := registerClass(promptClassName, windows.NewCallback(promptWndProc), windowIcon); err != nil {
		return fmt.Errorf("register dialog class: %w", err)
	}
	if err := a.createMainWindow(); err != nil {
		return err
	}
	a.taskbarCreated = registerWindowMessage("TaskbarCreated")
	return nil
}

// ---- tray icon ----

func (a *app) trayData(flags uint32) *notifyIconData {
	data := &notifyIconData{hwnd: a.hwnd, id: trayIconID, flags: flags, callbackMessage: wmTray}
	data.size = uint32(unsafe.Sizeof(*data))
	data.icon = a.iconIdle
	if a.streaming {
		data.icon = a.iconActive
	}
	copyU16(data.tip[:], appName+" - "+a.status)
	return data
}

func (a *app) addTrayIcon() {
	data := a.trayData(nifMessage | nifIcon | nifTip)
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(data)))
}

func (a *app) updateTrayIcon() {
	data := a.trayData(nifIcon | nifTip)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(data)))
}

func (a *app) removeTrayIcon() {
	data := a.trayData(0)
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(data)))
}

// notify shows a notification near the tray icon.
func (a *app) notify(title, text string, isError bool) {
	data := a.trayData(nifInfo)
	copyU16(data.infoTitle[:], title)
	copyU16(data.info[:], text)
	data.infoFlags = niifInfo
	if isError {
		data.infoFlags = niifError
	}
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(data)))
}

func (a *app) setStatus(status string) {
	a.status = status
	a.updateTrayIcon()
	a.refreshWindow()
}

// ---- UI thread queue ----

// call runs fn on the UI thread and does not wait.
func (a *app) call(fn func()) {
	a.queueMu.Lock()
	a.queue = append(a.queue, fn)
	a.queueMu.Unlock()
	postMessage(a.hwnd, wmUICall, 0, 0)
}

// callSync runs fn on the UI thread and waits until it is done. Do not use
// it from the UI thread.
func (a *app) callSync(fn func()) {
	done := make(chan struct{})
	a.call(func() {
		defer close(done)
		fn()
	})
	<-done
}

func (a *app) runQueued() {
	for {
		a.queueMu.Lock()
		if len(a.queue) == 0 {
			a.queueMu.Unlock()
			return
		}
		fn := a.queue[0]
		a.queue = a.queue[1:]
		a.queueMu.Unlock()
		fn()
	}
}

// ---- device list ----

func deviceKey(d airplay.AirPlayDevice) string {
	if d.DeviceID != "" {
		return strings.ToLower(d.DeviceID)
	}
	return net.JoinHostPort(d.IP, strconv.Itoa(d.Port))
}

// modelName makes a model code readable: "AppleTV14,1" becomes "Apple TV".
func modelName(model string) string {
	switch {
	case strings.HasPrefix(model, "AppleTV"):
		return "Apple TV"
	case strings.HasPrefix(model, "AudioAccessory"):
		return "HomePod"
	case strings.HasPrefix(model, "Mac"), strings.Contains(model, "Mac"):
		return "Mac"
	}
	return model
}

// rowLabel is the text of a device in the window and in the menu.
func rowLabel(row deviceRow, state string) string {
	d := row.device
	name := d.Name
	if name == "" {
		name = d.IP
	}
	details := []string{}
	if model := modelName(d.Model); model != "" {
		details = append(details, model)
	}
	if d.Name != "" {
		details = append(details, d.IP)
	}
	label := name
	if len(details) > 0 {
		label += "  (" + strings.Join(details, ", ") + ")"
	}
	switch {
	case row.active && state != "":
		label += "  -  " + state
	case !row.found:
		label += "  -  saved, not found now"
	}
	return label
}

// rebuildRows makes the device list from the search result and the saved
// devices. A saved device that the search did not find stays in the list.
func (a *app) rebuildRows() {
	var rows []deviceRow
	matched := make([]bool, len(a.settings.Known))
	changed := false
	for _, d := range a.devices {
		row := deviceRow{device: d, found: true}
		for i := range a.settings.Known {
			k := &a.settings.Known[i]
			if matched[i] || !mirror.SameDevice(k.device(), d) {
				continue
			}
			matched[i] = true
			row.known = true
			// The receiver can get a new address or name. Keep the newest.
			if d.IP != "" && (k.IP != d.IP || k.Port != d.Port || (d.Name != "" && k.Name != d.Name)) {
				k.IP, k.Port = d.IP, d.Port
				if d.Name != "" {
					k.Name = d.Name
				}
				changed = true
			}
			break
		}
		rows = append(rows, row)
	}
	for i, k := range a.settings.Known {
		if !matched[i] {
			rows = append(rows, deviceRow{device: k.device(), known: true})
		}
	}
	if s := a.session; s != nil {
		found := false
		for i := range rows {
			if mirror.SameDevice(rows[i].device, s.device) {
				rows[i].active = true
				found = true
				break
			}
		}
		if !found {
			rows = append([]deviceRow{{device: s.device, active: true, found: true}}, rows...)
		}
	}
	if len(rows) > maxDevices {
		rows = rows[:maxDevices]
	}
	a.rows = rows
	if changed {
		a.settings.save()
	}
	a.refreshWindow()
}

// rememberDevice puts the receiver of a session that operates into the
// saved list, as the first entry.
func (a *app) rememberDevice(d airplay.AirPlayDevice) {
	if d.IP == "" {
		return
	}
	entry := knownDevice{Name: d.Name, Model: d.Model, IP: d.IP, Port: d.Port, DeviceID: d.DeviceID,
		LastUsed: time.Now().Format(time.RFC3339)}
	known := []knownDevice{entry}
	for _, k := range a.settings.Known {
		if !mirror.SameDevice(k.device(), d) && len(known) < 50 {
			known = append(known, k)
		}
	}
	a.settings.Known = known
	a.settings.save()
	a.rebuildRows()
}

func (a *app) forgetDevice(d airplay.AirPlayDevice) {
	known := a.settings.Known[:0]
	for _, k := range a.settings.Known {
		if !mirror.SameDevice(k.device(), d) {
			known = append(known, k)
		}
	}
	a.settings.Known = known
	a.settings.save()
	a.rebuildRows()
}

// sessionState is the short state text of the active device.
func (a *app) sessionState() string {
	switch {
	case a.session == nil:
		return ""
	case a.streaming:
		return "connected"
	}
	return "connecting"
}

// ---- menus ----

func checked(on bool) uint32 {
	if on {
		return mfString | mfChecked
	}
	return mfString
}

// appendSettingsMenu adds the settings to a menu. The tray menu and the
// Settings button of the window use it.
func (a *app) appendSettingsMenu(menu uintptr) {
	displays := int(systemMetric(smCMonitors))
	if displays > maxDisplays {
		displays = maxDisplays
	}
	if displays > 1 {
		sub := createPopupMenu()
		for i := 0; i < displays; i++ {
			appendMenu(sub, checked(a.settings.Display == i), uintptr(idDisplay+i), "Display "+strconv.Itoa(i+1))
		}
		appendMenu(menu, mfPopup, sub, "Display")
	}

	pictureMenu := createPopupMenu()
	appendMenu(pictureMenu, checked(!a.settings.Picture1080p), idPicture4K, "4K, if the TV and the graphics card can do it")
	appendMenu(pictureMenu, checked(a.settings.Picture1080p), idPictureHD, "1080p")
	appendMenu(menu, mfPopup, pictureMenu, "Picture")

	encoderMenu := createPopupMenu()
	for i, choice := range encoderChoices {
		appendMenu(encoderMenu, checked(a.settings.Encoder == choice.value), uintptr(idEncoder+i), choice.label)
	}
	appendMenu(menu, mfPopup, encoderMenu, "Video encoder")

	fpsMenu := createPopupMenu()
	for i, fps := range fpsChoices {
		appendMenu(fpsMenu, checked(a.settings.FPS == fps), uintptr(idFPS+i), strconv.Itoa(fps)+" frames per second")
	}
	appendMenu(menu, mfPopup, fpsMenu, "Frame rate")

	soundMenu := createPopupMenu()
	for mode, label := range soundLabels {
		appendMenu(soundMenu, checked(a.settings.SoundMode == mode), uintptr(idSound+mode), label)
	}
	appendMenu(menu, mfPopup, soundMenu, "Sound")

	appendMenu(menu, checked(!a.settings.HideCursor), idCursor, "Show the mouse pointer")
	appendMenu(menu, mfSeparator, 0, "")
	appendMenu(menu, checked(a.settings.DebugLog), idDebugLog, "Write a detailed log")
	appendMenu(menu, mfString, idOpenLog, "Open the log file")
}

// trackMenu shows a popup menu at a screen position and runs the command.
func (a *app) trackMenu(menu uintptr, x, y int32) {
	// The window must be in the foreground, or the menu does not close when
	// the user clicks outside it.
	procSetForegroundWindow.Call(a.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmNoNotify|tpmReturnCmd,
		uintptr(x), uintptr(y), 0, a.hwnd, 0)
	postMessage(a.hwnd, 0, 0, 0)
	a.onCommand(int(cmd))
}

func (a *app) showTrayMenu() {
	menu := createPopupMenu()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenu(menu, mfString, idOpenUI, "Open winairplay")
	appendMenu(menu, mfString|mfGrayed, 0, a.status)
	appendMenu(menu, mfSeparator, 0, "")
	if len(a.rows) == 0 {
		text := "No AirPlay receiver found"
		if a.searching {
			text = "Searching for AirPlay receivers..."
		}
		appendMenu(menu, mfString|mfGrayed, 0, text)
	}
	for i, row := range a.rows {
		// "&" marks a keyboard shortcut in a menu. Show it as text.
		label := strings.ReplaceAll(rowLabel(row, ""), "&", "&&")
		appendMenu(menu, checked(row.active), uintptr(idDevice+i), "Mirror to "+label)
	}
	appendMenu(menu, mfString, idManualIP, "Mirror to an IP address...")
	appendMenu(menu, mfString, idRefresh, "Search again")
	appendMenu(menu, mfSeparator, 0, "")
	stopFlags := uint32(mfString)
	if a.session == nil {
		stopFlags |= mfGrayed
	}
	appendMenu(menu, stopFlags, idStop, "Stop mirroring")
	appendMenu(menu, mfSeparator, 0, "")
	a.appendSettingsMenu(menu)
	appendMenu(menu, mfString|mfGrayed, 0, appName+" "+version)
	appendMenu(menu, mfString, idExit, "Exit")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	a.trackMenu(menu, pt.x, pt.y)
}

func (a *app) onCommand(cmd int) {
	settingsChanged := func() {
		a.settings.save()
		if a.session != nil {
			a.notify(appName, "The new setting applies when you start mirroring again.", false)
		}
	}
	switch {
	case cmd == 0:
	case cmd == idOpenUI:
		a.showMainWindow()
	case cmd == idStop:
		a.stopSession()
	case cmd == idRefresh:
		a.requestDiscovery()
	case cmd == idManualIP:
		a.mirrorToAddress()
	case cmd == idOpenLog:
		a.openLog()
	case cmd == idExit:
		a.exit()
	case cmd == idCursor:
		a.settings.HideCursor = !a.settings.HideCursor
		settingsChanged()
	case cmd == idPicture4K, cmd == idPictureHD:
		a.settings.Picture1080p = cmd == idPictureHD
		settingsChanged()
	case cmd == idDebugLog:
		a.settings.DebugLog = !a.settings.DebugLog
		airplay.SetDebugMode(a.settings.DebugLog)
		a.settings.save()
	case cmd >= idDevice && cmd < idDevice+len(a.rows):
		a.startSession(a.rows[cmd-idDevice].device)
	case cmd >= idDisplay && cmd < idDisplay+maxDisplays:
		a.settings.Display = cmd - idDisplay
		settingsChanged()
	case cmd >= idFPS && cmd < idFPS+len(fpsChoices):
		a.settings.FPS = fpsChoices[cmd-idFPS]
		settingsChanged()
	case cmd >= idEncoder && cmd < idEncoder+len(encoderChoices):
		a.settings.Encoder = encoderChoices[cmd-idEncoder].value
		settingsChanged()
	case cmd >= idSound && cmd < idSound+len(soundLabels):
		// The sound output changes immediately, also during mirroring.
		a.settings.SoundMode = cmd - idSound
		airplay.SetAudioOutputMode(airplay.AudioOutputMode(a.settings.SoundMode))
		a.settings.save()
	}
}

func (a *app) openLog() {
	if a.logPath == "" {
		a.notify(appName, "The log file is not available.", true)
		return
	}
	if err := windows.ShellExecute(0, u16("open"), u16(a.logPath), nil, nil, swShow); err != nil {
		log.Printf("open the log file: %v", err)
	}
}

func (a *app) mirrorToAddress() {
	value, ok := a.prompt("Mirror to an IP address",
		"Enter the IP address of the Apple TV or the TV.\nYou can add a port, for example 192.168.1.50:7000.")
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return
	}
	device := airplay.AirPlayDevice{IP: value, Port: 7000}
	if host, portText, err := net.SplitHostPort(value); err == nil {
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			a.notify(appName, "The port in \""+value+"\" is not valid.", true)
			return
		}
		device = airplay.AirPlayDevice{IP: host, Port: port}
	}
	a.startSession(device)
}

func (a *app) exit() {
	if s := a.session; s != nil {
		s.cancel()
		if a.dialog == nil {
			// Give the session time to send TEARDOWN to the receiver and to
			// remove a mute of the PC. With an open input window the session
			// waits for this thread, so do not wait for it.
			select {
			case <-s.done:
			case <-time.After(3 * time.Second):
			}
		}
	}
	// Remove the icon while its window still exists.
	a.removeTrayIcon()
	procDestroyWindow.Call(a.hwnd)
}

// ---- discovery ----

func (a *app) requestDiscovery() {
	a.searching = true
	a.refreshWindow()
	select {
	case a.refresh <- struct{}{}:
	default:
	}
}

func (a *app) discoveryLoop() {
	for round := 0; ; round++ {
		var busy bool
		a.callSync(func() {
			a.searching = true
			busy = a.session != nil
			a.refreshWindow()
		})
		var devices []airplay.AirPlayDevice
		var err error
		// mDNS finds the receivers in most networks. If it finds nothing, a
		// scan of the local network addresses follows: at the start, when the
		// user selects "Search again", and then one time in each two minutes.
		// No scan runs during mirroring.
		forced := false
		select {
		case <-a.refresh:
			forced = true
		default:
		}
		if !busy && (forced || round%6 == 0) {
			var scanned bool
			devices, scanned, err = mirror.FindReceivers(context.Background(), 4*time.Second)
			if scanned {
				log.Printf("mDNS found no receiver; the network scan found %d", len(devices))
			}
		} else {
			devices, err = mirror.Discover(context.Background(), 4*time.Second)
		}
		if err != nil {
			log.Printf("discovery failed: %v", err)
		}
		a.call(func() {
			a.searching = false
			a.mergeDevices(devices)
		})
		select {
		case <-a.refresh:
			// Put the request back: the next round reads it.
			select {
			case a.refresh <- struct{}{}:
			default:
			}
		case <-time.After(20 * time.Second):
		}
	}
}

// mergeDevices puts a search result into the list. mDNS answers get lost at
// times, so a receiver stays in the list until three searches in a row did
// not find it.
func (a *app) mergeDevices(found []airplay.AirPlayDevice) {
	if a.missed == nil {
		a.missed = map[string]int{}
	}
	seen := map[string]bool{}
	merged := make([]airplay.AirPlayDevice, 0, len(found)+len(a.devices))
	for _, d := range found {
		if key := deviceKey(d); !seen[key] {
			seen[key] = true
			a.missed[key] = 0
			merged = append(merged, d)
		}
	}
	for _, d := range a.devices {
		key := deviceKey(d)
		if seen[key] {
			continue
		}
		a.missed[key]++
		if a.missed[key] < 3 {
			seen[key] = true
			merged = append(merged, d)
		} else {
			delete(a.missed, key)
		}
	}
	a.devices = merged
	a.rebuildRows()
}

// ---- sessions ----

func (a *app) stopSession() {
	if a.session == nil {
		return
	}
	a.session.cancel()
	if a.dialog != nil {
		a.dialog.cancel()
	}
	a.setStatus("Stopping")
}

func (a *app) startSession(device airplay.AirPlayDevice) {
	previous := a.session
	if previous != nil {
		previous.cancel()
		if a.dialog != nil {
			a.dialog.cancel()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &activeSession{device: device, cancel: cancel, done: make(chan struct{})}
	a.session = session
	a.streaming = false
	name := device.Name
	if name == "" {
		name = device.IP
	}
	a.status = "Connecting to " + name
	a.updateTrayIcon()
	a.rebuildRows()

	opts := mirror.Options{
		FPS:            a.settings.FPS,
		Encoder:        a.settings.Encoder,
		Display:        a.settings.Display,
		HideCursor:     a.settings.HideCursor,
		HighResolution: !a.settings.Picture1080p,
	}
	ui := mirror.UI{
		AskCredential: func(prompt mirror.Prompt, receiver string) (string, bool) {
			var value string
			var ok bool
			a.callSync(func() {
				if a.session != session {
					return
				}
				title, text := "AirPlay code", "Enter the PIN or the password of "+receiver+"."
				switch prompt {
				case mirror.PromptPIN:
					text = "Enter the PIN that " + receiver + " shows on its screen."
				case mirror.PromptPassword:
					title, text = "AirPlay password", "Enter the AirPlay password of "+receiver+"."
				}
				value, ok = a.prompt(title, text)
			})
			return strings.TrimSpace(value), ok
		},
		Status: func(message string) {
			a.call(func() {
				if a.session == session {
					a.setStatus(message)
				}
			})
		},
		Identified: func(identified airplay.AirPlayDevice) {
			a.call(func() {
				if a.session == session {
					session.device = identified
					a.rebuildRows()
				}
			})
		},
		Streaming: func(receiver, detail string) {
			a.call(func() {
				if a.session == session {
					a.streaming = true
					a.status = "Mirroring to " + receiver + " (" + detail + ")"
					a.updateTrayIcon()
					a.rememberDevice(session.device)
					a.notify(appName, "Mirroring to "+receiver+" started: "+detail+".", false)
				}
			})
		},
		Notice: func(message string) {
			a.call(func() {
				if a.session == session {
					a.notify(appName, shortText(message), false)
				}
			})
		},
	}

	go func() {
		defer close(session.done)
		if previous != nil {
			// Only one FFmpeg process must capture the screen at a time.
			<-previous.done
		}
		err := mirror.Run(ctx, device, opts, ui)
		cancel()
		if err != nil {
			log.Printf("session with %s ended: %v", device.IP, err)
		}
		a.call(func() {
			if a.session != session {
				return
			}
			a.session = nil
			a.streaming = false
			a.status = "Not connected"
			if err != nil && !errors.Is(err, mirror.ErrCancelled) {
				a.status = "Not connected. Last problem: " + shortText(err.Error())
				a.notify("Mirroring stopped", shortText(err.Error()), true)
			}
			a.updateTrayIcon()
			a.rebuildRows()
		})
	}()
}

// shortText makes a text that fits in a notification (255 characters).
func shortText(text string) string {
	if i := strings.Index(text, "; FFmpeg output:"); i > 0 {
		text = text[:i] + ". See the log file for the FFmpeg output."
	}
	runes := []rune(text)
	if len(runes) > 240 {
		text = string(runes[:237]) + "..."
	}
	return text
}
