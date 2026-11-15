// Package mirror runs one screen mirroring session: connect, pair, FairPlay
// setup, capture start and video streaming. It is the non-interactive core of
// the tray application and of the command line program.
//
// The session flow follows cmd/doubletake/main.go of the doubletake project
// (LGPL-3.0-or-later). Here it returns errors and asks for credentials
// through a callback, so that a GUI can use it.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"runtime"
	"strings"
	"time"

	"github.com/bsdrip/winairplay/internal/airplay"
)

// Options are the settings of one mirroring session.
type Options struct {
	FPS        int    // frames per second, 0 = 30
	Bitrate    int    // kbps, 0 = automatic
	Encoder    string // auto, none, nvenc, qsv, amf, mf, openh264
	Display    int    // zero-based monitor index
	HideCursor bool
	TestMode   bool // send a test pattern and a test tone instead of the screen
	// HighResolution selects HEVC video at up to 3840 x 2160. Without it the
	// session uses H.264 at the nominal size of the receiver (usually
	// 1920 x 1080). If the receiver or this computer cannot do HEVC, the
	// session starts again with H.264.
	HighResolution bool
	NoSound        bool // do not send the sound of the PC
	ForcePair      bool // ignore saved credentials and pair again

	// CredBackend is "keyring", "file" or empty. Empty selects the system
	// keyring (Windows Credential Manager) on Windows and the file elsewhere.
	CredBackend string
	CredFile    string // empty = default path

	// Code is an optional PIN or password. When it is empty and the receiver
	// needs one, the session calls AskCredential.
	Code string
}

// Prompt tells the user interface which type of credential to ask for.
type Prompt int

const (
	// PromptPIN: the receiver shows a PIN on its screen now.
	PromptPIN Prompt = iota
	// PromptPassword: the receiver has a fixed AirPlay password.
	PromptPassword
	// PromptPINOrPassword: the receiver did not say which of the two it needs.
	PromptPINOrPassword
)

// UI connects a session to the user interface. All functions can be nil.
type UI struct {
	// AskCredential returns the value that the user typed. ok is false when
	// the user cancelled.
	AskCredential func(prompt Prompt, receiver string) (value string, ok bool)
	// Status reports progress in a short sentence.
	Status func(message string)
	// Streaming is called one time, when video starts to flow. detail
	// describes the stream, for example "3840 x 2160 HEVC".
	Streaming func(receiver, detail string)
	// Notice reports a problem that does not stop the session, for example
	// that the receiver cannot do 4K and the session uses 1080p.
	Notice func(message string)
}

func (ui UI) notice(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	log.Print(message)
	if ui.Notice != nil {
		ui.Notice(message)
	}
}

func (ui UI) status(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	log.Print(message)
	if ui.Status != nil {
		ui.Status(message)
	}
}

// ErrCancelled is returned when the user cancelled a credential prompt.
var ErrCancelled = errors.New("cancelled by the user")

// Discover searches the local network for AirPlay receivers.
func Discover(ctx context.Context, timeout time.Duration) ([]airplay.AirPlayDevice, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	devices, err := airplay.DiscoverAirPlayDevices(discoverCtx)
	if err != nil {
		return nil, err
	}
	// Keep only receivers that accept screen mirroring, when the receiver
	// tells this in its advertisement.
	screens := devices[:0]
	for _, device := range devices {
		if device.Features == 0 || device.SupportsScreen() {
			screens = append(screens, device)
		}
	}
	devices = screens
	sortDevices(devices)
	return devices, nil
}

func newCredentialStore(opts Options) (*airplay.CredentialStore, string, error) {
	backend := opts.CredBackend
	if backend == "" {
		backend = "file"
		if runtime.GOOS == "windows" {
			backend = "keyring"
		}
	}
	path := opts.CredFile
	if path == "" {
		path = airplay.DefaultCredentialsPath()
	}
	if backend == "keyring" {
		keyring, err := airplay.NewKeyringBackend()
		if err == nil {
			return airplay.NewCredentialStoreWithBackend(keyring), "keyring", nil
		}
		if opts.CredBackend == "keyring" {
			return nil, "", err
		}
		log.Printf("system keyring is not available (%v); using %s", err, path)
	}
	store, err := airplay.NewCredentialStore(path)
	return store, "file", err
}

// session holds the state that the steps of Run share.
type session struct {
	ctx        context.Context
	opts       Options
	ui         UI
	device     airplay.AirPlayDevice
	fromMDNS   bool
	client     *airplay.AirPlayClient
	info       *airplay.ReceiverInfo
	credential string
	store      *airplay.CredentialStore
}

func (s *session) newClient() *airplay.AirPlayClient {
	if s.fromMDNS {
		return airplay.NewAirPlayClientForDevice(s.device)
	}
	return airplay.NewAirPlayClient(s.device.IP, s.device.Port)
}

func (s *session) connect() error {
	if s.client != nil {
		_ = s.client.Close()
	}
	s.client = s.newClient()
	s.client.SetPassword(s.credential)
	if err := s.client.Connect(s.ctx); err != nil {
		return fmt.Errorf("connect to %s: %w", net.JoinHostPort(s.device.IP, fmt.Sprint(s.device.Port)), err)
	}
	info, err := s.client.GetInfo()
	if err != nil {
		return fmt.Errorf("read receiver information: %w", err)
	}
	s.info = info
	return nil
}

func (s *session) receiverName() string {
	if s.info != nil && s.info.Name != "" {
		return s.info.Name
	}
	if s.device.Name != "" {
		return s.device.Name
	}
	return s.device.IP
}

func (s *session) ask(prompt Prompt) (string, error) {
	if s.ui.AskCredential == nil {
		return "", fmt.Errorf("the receiver needs a PIN or password")
	}
	value, ok := s.ui.AskCredential(prompt, s.receiverName())
	if !ok {
		return "", ErrCancelled
	}
	if value == "" {
		return "", fmt.Errorf("the PIN or password is empty")
	}
	return value, nil
}

// pairWithCredential does a full pair-setup. When value is empty it makes the
// receiver show its PIN and asks the user.
func (s *session) pairWithCredential(value string, expectPIN bool) error {
	if value == "" {
		prompt := PromptPINOrPassword
		if err := s.client.StartPINDisplay(); err != nil {
			log.Printf("warning: the receiver did not start its PIN display: %v", err)
		} else if expectPIN {
			prompt = PromptPIN
		}
		if !expectPIN && s.info.RequiresPassword() {
			prompt = PromptPassword
		}
		s.ui.status("Waiting for the code from %s", s.receiverName())
		var err error
		if value, err = s.ask(prompt); err != nil {
			return err
		}
	}
	// AirPlay uses the same value for pair-setup and, on receivers with a
	// password, for the HTTP Digest challenge. Keep it for reconnects.
	s.credential = value
	s.client.SetPassword(value)
	if err := s.client.Pair(s.ctx, value); err != nil {
		return fmt.Errorf("pairing failed: %w", err)
	}
	if s.client.PairKeys != nil {
		if err := s.store.SavePairing(s.info.DeviceID, s.client.PairingID,
			s.client.PairKeys.Ed25519Public, s.client.PairKeys.Ed25519Private,
			s.client.PairingProtocol()); err != nil {
			log.Printf("warning: could not save the pairing credentials: %v", err)
		} else {
			log.Printf("pairing credentials saved")
		}
	}
	return nil
}

// pairFresh pairs without saved credentials.
func (s *session) pairFresh() error {
	info := s.info
	switch {
	case info.RequiredPairingCredential() == airplay.PairingCredentialPassword:
		return s.pairWithCredential("", false)
	case info.RequiredPairingCredential() == airplay.PairingCredentialPIN:
		return s.pairWithCredential("", true)
	}
	err := s.client.Pair(s.ctx, "")
	if err == nil {
		return nil
	}
	if info.RequiresPassword() {
		return fmt.Errorf("transient pairing failed for a receiver with a password: %w", err)
	}
	log.Printf("transient pairing failed: %v; asking for pairing credentials", err)
	// A failed exchange can leave receiver state on the socket. Use a new
	// connection for the credential pairing.
	if err := s.connect(); err != nil {
		return err
	}
	return s.pairWithCredential("", false)
}

func (s *session) pair() error {
	info := s.info
	if s.opts.ForcePair {
		if info.RequiresPassword() && info.RequiredPairingCredential() == airplay.PairingCredentialNone {
			// The fixed password of a modern receiver is for HTTP Digest only.
			if err := s.client.Pair(s.ctx, ""); err != nil {
				return fmt.Errorf("transient pairing failed for a receiver with a password: %w", err)
			}
			return nil
		}
		return s.pairWithCredential(s.credential, !info.RequiresPassword())
	}
	saved := s.store.Lookup(info.DeviceID)
	if saved != nil && saved.HasPairingCredentials() {
		err := s.client.RestorePairingCredentials(saved)
		if err == nil {
			err = s.client.PairVerify(s.ctx)
		}
		if err == nil {
			log.Printf("pair-verify with saved credentials succeeded")
			return nil
		}
		log.Printf("pair-verify with saved credentials failed: %v", err)
		if err := s.connect(); err != nil {
			return err
		}
	}
	return s.pairFresh()
}

// Run mirrors the screen to the device until ctx is cancelled or the stream
// stops. It returns nil when ctx was cancelled.
func Run(ctx context.Context, device airplay.AirPlayDevice, opts Options, ui UI) error {
	stage, err := run(ctx, device, opts, ui)
	if err != nil && opts.HighResolution && stage == stageVideo && ctx.Err() == nil && !errors.Is(err, ErrCancelled) {
		// 4K did not start. Use a new connection for the 1080p session,
		// because the failed setup can leave state on the receiver.
		ui.notice("4K is not available (%v). Using 1080p.", err)
		opts.HighResolution = false
		_, err = run(ctx, device, opts, ui)
	}
	if ctx.Err() != nil && !errors.Is(err, ErrCancelled) {
		return nil
	}
	return err
}

// sessionStage is the last part of a session that started.
type sessionStage int

const (
	stageConnect   sessionStage = iota // connect, pairing, FairPlay
	stageVideo                         // capture preparation and stream setup
	stageStreaming                     // video flows
)

// run does one session and reports how far it came.
func run(parent context.Context, device airplay.AirPlayDevice, opts Options, ui UI) (stage sessionStage, err error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	if opts.FPS <= 0 {
		opts.FPS = 30
	}
	if device.Port == 0 {
		device.Port = 7000
	}
	if err := airplay.ValidateHWAccel(opts.Encoder); err != nil {
		return stage, err
	}
	store, backend, storeErr := newCredentialStore(opts)
	if err = storeErr; err != nil {
		return stage, fmt.Errorf("open the credential store: %w", err)
	}
	log.Printf("credential store: %s", backend)

	s := &session{
		ctx: ctx, opts: opts, ui: ui, device: device,
		// A device from discovery has its mDNS TXT data. A device that the
		// user gave as an address has only IP and port.
		fromMDNS:   device.RawTXT != nil,
		credential: opts.Code,
		store:      store,
	}
	defer func() {
		if s.client != nil {
			_ = s.client.Close()
		}
	}()

	ui.status("Connecting to %s", s.receiverName())
	if err := s.connect(); err != nil {
		return stage, err
	}
	log.Printf("connected to %s (model %s)", s.info.Name, s.info.Model)
	if s.credential == "" && s.info.RequiresPassword() {
		if s.credential, err = s.ask(PromptPassword); err != nil {
			return stage, err
		}
		s.client.SetPassword(s.credential)
	}

	ui.status("Pairing with %s", s.receiverName())
	if err := s.pair(); err != nil {
		return stage, err
	}
	log.Printf("pairing complete")

	if s.client.FpEkey == nil {
		if err := s.client.FairPlaySetup(ctx); err != nil {
			if !errors.Is(err, airplay.ErrFairPlayUnsupported) {
				return stage, fmt.Errorf("FairPlay setup failed: %w", err)
			}
			log.Printf("FairPlay SAP is not supported (%v); continuing with pair-verify setup", err)
		} else {
			log.Printf("FairPlay setup complete")
		}
	}

	codec := airplay.VideoCodecH264
	if opts.HighResolution {
		codec = airplay.VideoCodecHEVC
	}
	streamCfg := airplay.StreamConfig{
		FPS:        opts.FPS,
		Bitrate:    opts.Bitrate,
		VideoCodec: codec,
		NoAudio:    opts.NoSound,
	}
	captureCfg := airplay.CaptureConfig{
		FPS:        opts.FPS,
		Bitrate:    opts.Bitrate,
		HWAccel:    opts.Encoder,
		VideoCodec: codec,
		ShowCursor: !opts.HideCursor,
		Display:    opts.Display,
	}
	stage = stageVideo
	var preparation *airplay.CapturePreparation
	if opts.TestMode {
		preparation, err = airplay.PrepareTestCapture(ctx, captureCfg)
	} else {
		preparation, err = airplay.PrepareCapture(ctx, captureCfg)
	}
	if err != nil {
		return stage, fmt.Errorf("prepare screen capture: %w", err)
	}
	defer preparation.Close()

	var (
		capture       *airplay.ScreenCapture
		broadcast     *airplay.BroadcastCapture
		broadcastDone chan error
		startedWidth  int
		startedHeight int
	)
	defer func() {
		if capture != nil {
			capture.Stop()
			<-broadcastDone
		}
	}()
	var videoLead time.Duration
	prepareVideo := func(width, height int, selected airplay.VideoCodec) (airplay.VideoPreparationResult, error) {
		if capture != nil {
			if selected != codec {
				return airplay.VideoPreparationResult{}, fmt.Errorf("receiver changed the video codec to %s during setup", selected)
			}
			if width != startedWidth || height != startedHeight {
				log.Printf("receiver changed the canvas to %dx%d after capture started; keeping %dx%d", width, height, startedWidth, startedHeight)
			}
			return airplay.VideoPreparationResult{MinimumVideoLead: videoLead}, nil
		}
		ui.status("Starting screen capture")
		started, err := preparation.StartWithCodec(width, height, selected)
		if err != nil {
			return airplay.VideoPreparationResult{}, err
		}
		if selected == airplay.VideoCodecHEVC && !airplay.HasExplicitTargetLatency() {
			// HEVC frames have capture time stamps. Measure how late the
			// encoder gives them, so the receiver schedules them in time.
			videoLead, err = airplay.MeasureVideoCaptureLatency(ctx, started, opts.FPS)
			if err != nil {
				started.Stop()
				return airplay.VideoPreparationResult{}, fmt.Errorf("the HEVC encoder is too slow for this picture size: %w", err)
			}
			log.Printf("HEVC encoder needs %v video lead", videoLead)
		}
		capture = started
		broadcast = airplay.NewBroadcastCaptureWithFrameRate(started, opts.FPS)
		broadcastDone = make(chan error, 1)
		startedWidth, startedHeight = width, height
		go func(active *airplay.BroadcastCapture, done chan<- error) {
			done <- active.Run()
		}(broadcast, broadcastDone)
		log.Printf("screen capture started at %dx%d using %s", width, height, selected)
		return airplay.VideoPreparationResult{MinimumVideoLead: videoLead}, nil
	}

	ui.status("Starting the mirror session")
	mirrorSession, err := s.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
	if err != nil && !streamCfg.NoAudio && capture == nil && strings.Contains(err.Error(), "negotiate screen audio") {
		// The receiver accepts only an audio format that this program cannot
		// make. This check is done before the first request, so the same
		// connection can continue without sound.
		ui.notice("The receiver does not accept the sound format of winairplay. Mirroring without sound.")
		streamCfg.NoAudio = true
		mirrorSession, err = s.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
	}
	if errors.Is(err, airplay.ErrCredentialsRequired) {
		// Some receivers show that they have a password only when they
		// challenge the first media SETUP. Ask and try one more time.
		if s.credential, err = s.ask(PromptPINOrPassword); err != nil {
			return stage, err
		}
		s.client.SetPassword(s.credential)
		mirrorSession, err = s.client.SetupMirrorWithCalibratedVideoPreparation(ctx, streamCfg, prepareVideo)
	}
	if err != nil {
		return stage, fmt.Errorf("mirror setup failed: %w", err)
	}
	defer mirrorSession.Close()
	if capture == nil || broadcast == nil {
		return stage, fmt.Errorf("mirror setup completed without video capture")
	}
	log.Printf("mirror session ready (data port %d)", mirrorSession.DataPort)

	go func() {
		<-ctx.Done()
		capture.Stop()
		mirrorSession.Close()
	}()

	if !streamCfg.NoAudio && mirrorSession.HasAudio() {
		audio, audioErr := airplay.StartAudioCapture(ctx, opts.TestMode, mirrorSession.AudioCodec())
		if audioErr != nil {
			ui.notice("No sound: %v", audioErr)
		} else {
			defer audio.Stop()
			go func() {
				if err := mirrorSession.StreamAudio(ctx, audio, mirrorSession.AudioStream()); err != nil && ctx.Err() == nil {
					ui.notice("The sound stopped: %v", err)
				}
			}()
			log.Printf("sound capture started")
		}
	} else if !streamCfg.NoAudio {
		log.Printf("no sound: the receiver gave no audio ports")
	}

	videoSink, err := broadcast.AddBackpressuredSink()
	if err != nil {
		return stage, fmt.Errorf("attach the video stream: %w", err)
	}
	defer videoSink.Close()
	ui.status("Mirroring to %s", s.receiverName())
	stage = stageStreaming
	if ui.Streaming != nil {
		ui.Streaming(s.receiverName(), fmt.Sprintf("%d x %d %s", startedWidth, startedHeight, strings.ToUpper(string(codec))))
	}
	if err := mirrorSession.StreamFrames(ctx, videoSink.AsCapture(), 0); err != nil && ctx.Err() == nil {
		return stage, fmt.Errorf("streaming stopped: %w", err)
	}
	if ctx.Err() == nil {
		// StreamFrames returned without an error and nobody asked to stop:
		// the capture or the receiver ended the stream.
		select {
		case captureErr := <-broadcastDone:
			broadcastDone <- captureErr
			if captureErr != nil {
				return stage, fmt.Errorf("screen capture stopped: %w", captureErr)
			}
		default:
		}
		return stage, fmt.Errorf("the stream ended")
	}
	return stage, nil
}
