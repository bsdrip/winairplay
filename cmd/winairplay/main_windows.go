//go:build windows

// winairplay is a Windows tray application that mirrors the screen to an
// Apple TV or another AirPlay receiver.
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
	appName = "winairplay"

	wmTray   = wmApp + 1 // message from the tray icon
	wmUICall = wmApp + 2 // run queued functions on the UI thread

	trayIconID = 1

	idStop      = 1
	idRefresh   = 2
	idManualIP  = 3
	idOpenLog   = 4
	idExit      = 5
	idCursor    = 6
	idSound     = 400 // + airplay.AudioOutputMode
	idDebugLog  = 8
	idPicture4K = 9
	idPictureHD = 10
	idEncoder   = 100  // + index in encoderChoices
	idFPS       = 200  // + index in fpsChoices
	idDisplay   = 300  // + zero-based display index
	idDevice    = 1000 // + index in the device list
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
	NoSound bool `json:"noSound,omitempty"`
	DebugLog     bool `json:"debugLog"`
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

type app struct {
	hwnd           uintptr
	iconIdle       uintptr
	iconActive     uintptr
	taskbarCreated uint32
	logPath        string

	// The fields below are used only on the UI thread.
	settings  settings
	devices   []airplay.AirPlayDevice
	missed    map[string]int // scans in a row that did not find a device
	searching bool
	session   *activeSession
	streaming bool
	status    string
	dialog    *promptDialog

	queueMu sync.Mutex
	queue   []func()

	refresh chan struct{}
}

var theApp *app

func main() {
	// Win32 windows belong to the thread that made them.
	runtime.LockOSThread()

	mutexName, _ := windows.UTF16PtrFromString("Local\\winairplay-single-instance")
	if _, err := windows.CreateMutex(nil, false, mutexName); errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		messageBox(appName, "winairplay is already running. Look for its icon in the notification area of the taskbar.", mbIconInfo)
		return
	}

	a := &app{settings: loadSettings(), status: "Not connected", refresh: make(chan struct{}, 1)}
	theApp = a
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
	go a.discoveryLoop()

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	log.Printf("%s stopped", appName)
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
	if err := registerClass("WinAirPlayMain", windows.NewCallback(mainWndProc), a.iconIdle); err != nil {
		return fmt.Errorf("register window class: %w", err)
	}
	if err := registerClass(promptClassName, windows.NewCallback(promptWndProc), a.iconIdle); err != nil {
		return fmt.Errorf("register dialog class: %w", err)
	}
	// The main window is never shown. It receives the tray icon messages.
	a.hwnd = createWindow(0, "WinAirPlayMain", appName, wsPopup, 0, 0, 0, 0, 0, 0)
	if a.hwnd == 0 {
		return fmt.Errorf("create the main window")
	}
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(u16("TaskbarCreated"))))
	a.taskbarCreated = uint32(r)
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

func mainWndProc(hwnd, rawMessage, wParam, lParam uintptr) uintptr {
	a := theApp
	message := uint32(rawMessage)
	switch {
	case a == nil:
	case message == wmTray:
		if lParam == wmLButtonUp || lParam == wmRButtonUp {
			a.showMenu()
		}
		return 0
	case message == wmUICall:
		a.runQueued()
		return 0
	case message == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	case a != nil && a.taskbarCreated != 0 && message == a.taskbarCreated:
		// Explorer started again: the icon must be added again.
		a.addTrayIcon()
		return 0
	}
	return defWindowProc(hwnd, message, wParam, lParam)
}

// ---- menu ----

func deviceLabel(d airplay.AirPlayDevice) string {
	name := d.Name
	if name == "" {
		name = d.IP
	}
	// "&" marks a keyboard shortcut in a menu. Show it as text.
	return strings.ReplaceAll(name, "&", "&&")
}

func (a *app) showMenu() {
	menu := createPopupMenu()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenu(menu, mfString|mfGrayed, 0, a.status)
	appendMenu(menu, mfSeparator, 0, "")
	checked := func(on bool) uint32 {
		if on {
			return mfString | mfChecked
		}
		return mfString
	}

	devices := a.devices
	if len(devices) > maxDevices {
		devices = devices[:maxDevices]
	}
	if len(devices) == 0 {
		text := "No AirPlay receiver found"
		if a.searching {
			text = "Searching for AirPlay receivers..."
		}
		appendMenu(menu, mfString|mfGrayed, 0, text)
	}
	for i, d := range devices {
		flags := uint32(mfString)
		if a.session != nil && mirror.SameDevice(a.session.device, d) {
			flags |= mfChecked
		}
		appendMenu(menu, flags, uintptr(idDevice+i), "Mirror to "+deviceLabel(d))
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

	displays := int(systemMetric(smCMonitors))
	if displays < 1 {
		displays = 1
	}
	if displays > maxDisplays {
		displays = maxDisplays
	}
	if displays > 1 {
		sub := createPopupMenu()
		for i := 0; i < displays; i++ {
			flags := uint32(mfString)
			if a.settings.Display == i {
				flags |= mfChecked
			}
			appendMenu(sub, flags, uintptr(idDisplay+i), "Display "+strconv.Itoa(i+1))
		}
		appendMenu(menu, mfPopup, sub, "Display")
	}

	pictureMenu := createPopupMenu()
	appendMenu(pictureMenu, checked(!a.settings.Picture1080p), idPicture4K, "4K, if the TV and the graphics card can do it")
	appendMenu(pictureMenu, checked(a.settings.Picture1080p), idPictureHD, "1080p")
	appendMenu(menu, mfPopup, pictureMenu, "Picture")

	encoderMenu := createPopupMenu()
	for i, choice := range encoderChoices {
		flags := uint32(mfString)
		if a.settings.Encoder == choice.value {
			flags |= mfChecked
		}
		appendMenu(encoderMenu, flags, uintptr(idEncoder+i), choice.label)
	}
	appendMenu(menu, mfPopup, encoderMenu, "Video encoder")

	fpsMenu := createPopupMenu()
	for i, fps := range fpsChoices {
		flags := uint32(mfString)
		if a.settings.FPS == fps {
			flags |= mfChecked
		}
		appendMenu(fpsMenu, flags, uintptr(idFPS+i), strconv.Itoa(fps)+" frames per second")
	}
	appendMenu(menu, mfPopup, fpsMenu, "Frame rate")

	cursorFlags := uint32(mfString)
	if !a.settings.HideCursor {
		cursorFlags |= mfChecked
	}
	appendMenu(menu, cursorFlags, idCursor, "Show the mouse pointer")
	soundMenu := createPopupMenu()
	for mode, label := range soundLabels {
		appendMenu(soundMenu, checked(a.settings.SoundMode == mode), uintptr(idSound+mode), label)
	}
	appendMenu(menu, mfPopup, soundMenu, "Sound")
	appendMenu(menu, mfSeparator, 0, "")
	appendMenu(menu, checked(a.settings.DebugLog), idDebugLog, "Write a detailed log")
	appendMenu(menu, mfString, idOpenLog, "Open the log file")
	appendMenu(menu, mfString|mfGrayed, 0, appName+" "+version)
	appendMenu(menu, mfString, idExit, "Exit")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// The window must be in the foreground, or the menu does not close when
	// the user clicks outside it.
	procSetForegroundWindow.Call(a.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmNoNotify|tpmReturnCmd,
		uintptr(pt.x), uintptr(pt.y), 0, a.hwnd, 0)
	postMessage(a.hwnd, 0, 0, 0)
	a.onCommand(int(cmd), devices)
}

func (a *app) onCommand(cmd int, devices []airplay.AirPlayDevice) {
	settingsChanged := func() {
		a.settings.save()
		if a.session != nil {
			a.notify(appName, "The new setting applies when you start mirroring again.", false)
		}
	}
	switch {
	case cmd == 0:
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
	case cmd >= idSound && cmd < idSound+len(soundLabels):
		// The sound output changes immediately, also during mirroring.
		a.settings.SoundMode = cmd - idSound
		airplay.SetAudioOutputMode(airplay.AudioOutputMode(a.settings.SoundMode))
		a.settings.save()
	case cmd == idPicture4K, cmd == idPictureHD:
		a.settings.Picture1080p = cmd == idPictureHD
		settingsChanged()
	case cmd == idDebugLog:
		a.settings.DebugLog = !a.settings.DebugLog
		airplay.SetDebugMode(a.settings.DebugLog)
	airplay.SetAudioOutputMode(airplay.AudioOutputMode(a.settings.SoundMode))
		a.settings.save()
	case cmd >= idDevice && cmd < idDevice+len(devices):
		a.startSession(devices[cmd-idDevice])
	case cmd >= idDisplay && cmd < idDisplay+maxDisplays:
		a.settings.Display = cmd - idDisplay
		settingsChanged()
	case cmd >= idFPS && cmd < idFPS+len(fpsChoices):
		a.settings.FPS = fpsChoices[cmd-idFPS]
		settingsChanged()
	case cmd >= idEncoder && cmd < idEncoder+len(encoderChoices):
		a.settings.Encoder = encoderChoices[cmd-idEncoder].value
		settingsChanged()
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
		"Enter the IP address of the Apple TV.\nYou can add a port, for example 192.168.1.50:7000.")
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
			// Give the session time to send TEARDOWN to the receiver. With
			// an open input window the session waits for this thread, so do
			// not wait for it.
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
			a.requestDiscovery()
		case <-time.After(20 * time.Second):
		}
	}
}

func deviceKey(d airplay.AirPlayDevice) string {
	if d.DeviceID != "" {
		return strings.ToLower(d.DeviceID)
	}
	return net.JoinHostPort(d.IP, strconv.Itoa(d.Port))
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
	if len(a.devices) == 0 && len(merged) > 0 && a.session == nil {
		a.setStatus(fmt.Sprintf("Not connected - %d AirPlay receiver(s) found", len(merged)))
	}
	a.devices = merged
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
	a.setStatus("Connecting to " + deviceLabel(device))

	opts := mirror.Options{
		FPS:        a.settings.FPS,
		Encoder:    a.settings.Encoder,
		Display:    a.settings.Display,
		HideCursor: a.settings.HideCursor,

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
		Streaming: func(receiver, detail string) {
			a.call(func() {
				if a.session == session {
					a.streaming = true
					a.setStatus("Mirroring to " + receiver + " (" + detail + ")")
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
			a.setStatus("Not connected")
			if err != nil && !errors.Is(err, mirror.ErrCancelled) {
				a.notify("Mirroring stopped", shortError(err), true)
			}
		})
	}()
}

// shortError makes an error text that fits in a notification (255 characters).
func shortError(err error) string {
	return shortText(err.Error())
}

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

// ---- text input dialog ----

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
	const width, height = 380, 190
	x := (systemMetric(smCxScreen) - width) / 2
	y := (systemMetric(smCyScreen) - height) / 2
	d := &promptDialog{}
	d.hwnd = createWindow(wsExTopmost|wsExDlgFrame, promptClassName, title,
		wsPopup|wsCaption|wsSysMenu, x, y, width, height, 0, 0)
	if d.hwnd == 0 {
		log.Printf("could not create the input window")
		return "", false
	}
	a.dialog = d
	defer func() { a.dialog = nil }()

	label := createWindow(0, "STATIC", text, wsChild|wsVisible|ssLeft, 16, 14, width-40, 50, d.hwnd, 0)
	d.edit = createWindow(0, "EDIT", "", wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, 16, 70, width-48, 24, d.hwnd, idEdit)
	okButton := createWindow(0, "BUTTON", "OK", wsChild|wsVisible|wsTabStop|bsDefPushButton, width-212, 108, 90, 28, d.hwnd, idOK)
	cancelButton := createWindow(0, "BUTTON", "Cancel", wsChild|wsVisible|wsTabStop, width-116, 108, 90, 28, d.hwnd, idCancel)
	font, _, _ := procGetStockObject.Call(defaultGUIFont)
	for _, control := range []uintptr{label, d.edit, okButton, cancelButton} {
		procSendMessageW.Call(control, wmSetFont, font, 1)
	}

	procShowWindow.Call(d.hwnd, swShow)
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
