package mirror

import (
	"context"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsdrip/winairplay/internal/airplay"
)

// requireFFmpeg skips the test when FFmpeg with x264 is not installed, and
// selects the FFmpeg capture backend.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx264") {
		t.Skip("FFmpeg has no libx264 encoder")
	}
	t.Setenv("WINAIRPLAY_CAPTURE", "ffmpeg")
}

// loopbackReceiver is a test receiver on the loopback interface together
// with the options of a sender that sends the FFmpeg test pattern to it.
type loopbackReceiver struct {
	server *airplay.ReceiverServer
	device airplay.AirPlayDevice
	opts   Options
	pin    string
	// detail and notices are from the last session.
	detail     string
	notices    []string
	identified airplay.AirPlayDevice
}

func startLoopbackReceiver(t *testing.T, profile airplay.ReceiverProfile, auth airplay.ReceiverAuthMode, pin string) *loopbackReceiver {
	t.Helper()
	server, err := airplay.NewReceiverServer(airplay.ReceiverConfig{
		ListenAddress: "127.0.0.1:0",
		Profile:       profile,
		Auth:          auth,
		Code:          pin,
	})
	if err != nil {
		t.Fatalf("start the test receiver: %v", err)
	}
	serveCtx, stopServer := context.WithCancel(context.Background())
	go func() { _ = server.Serve(serveCtx) }()
	t.Cleanup(func() {
		stopServer()
		_ = server.Close()
	})
	host, portText, _ := strings.Cut(server.Addr().String(), ":")
	port, _ := strconv.Atoi(portText)
	return &loopbackReceiver{
		server: server,
		device: airplay.AirPlayDevice{IP: host, Port: port},
		pin:    pin,
		opts: Options{
			FPS:         30,
			Bitrate:     2000,
			Encoder:     "none",
			TestMode:    true,
			CredBackend: "file",
			CredFile:    filepath.Join(t.TempDir(), "credentials.json"),
		},
	}
}

// runSession mirrors until the receiver counted 30 more video packets, then
// stops the session. progress selects the counter that must increase.
func (r *loopbackReceiver) runSession(t *testing.T, name string, wantPrompts int32, progress func(airplay.ReceiverStats) uint64) {
	t.Helper()
	before := progress(r.server.Stats())
	r.detail, r.notices = "", nil
	var prompts atomic.Int32
	streaming := make(chan struct{}, 1)
	ui := UI{
		AskCredential: func(Prompt, string) (string, bool) {
			prompts.Add(1)
			return r.pin, true
		},
		Streaming:  func(_, detail string) { r.detail = detail; streaming <- struct{}{} },
		Notice:     func(message string) { r.notices = append(r.notices, message) },
		Identified: func(device airplay.AirPlayDevice) { r.identified = device },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Run(ctx, r.device, r.opts, ui) }()

	select {
	case <-streaming:
	case err := <-result:
		t.Fatalf("%s: session ended before streaming: %v", name, err)
	case <-ctx.Done():
		t.Fatalf("%s: streaming did not start", name)
	}
	deadline := time.Now().Add(20 * time.Second)
	for progress(r.server.Stats()) < before+30 {
		if time.Now().After(deadline) {
			t.Fatalf("%s: receiver got too few video packets: %+v", name, r.server.Stats())
		}
		select {
		case err := <-result:
			t.Fatalf("%s: session ended during streaming: %v", name, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("%s: a cancelled session must return nil, got %v", name, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: session did not stop", name)
	}
	stats := r.server.Stats()
	if stats.VideoCryptoErrors != 0 {
		t.Errorf("%s: receiver could not decrypt %d packets", name, stats.VideoCryptoErrors)
	}
	if stats.VideoWidth == 0 || stats.VideoHeight == 0 {
		t.Errorf("%s: receiver saw no video dimensions: %+v", name, stats)
	}
	if got := prompts.Load(); got != wantPrompts {
		t.Errorf("%s: %d credential prompts, want %d", name, got, wantPrompts)
	}
}

// TestRunAgainstModernReceiver runs full sessions against a receiver that
// behaves as a current Apple TV: PIN pairing, FairPlay setup, FFmpeg capture
// of a test pattern and the encrypted video stream. The second session must
// connect with the saved credentials and must not ask for the PIN again.
// This receiver profile counts the video packets but does not decrypt them.
func TestRunAgainstModernReceiver(t *testing.T) {
	requireFFmpeg(t)
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileModern, airplay.ReceiverAuthPIN, "4821")
	packets := func(s airplay.ReceiverStats) uint64 { return s.VideoPackets }
	receiver.runSession(t, "first session", 1, packets)
	if id := receiver.identified; id.Name == "" || id.Model != "AppleTV14,1" || id.DeviceID == "" || id.IP != receiver.device.IP {
		t.Errorf("identified device = %+v, want name, model and device ID of the receiver", id)
	}
	if receiver.detail != "1920 x 1080 H264" {
		t.Errorf("first session streams %q, want 1920 x 1080 H264", receiver.detail)
	}
	receiver.runSession(t, "second session", 0, packets)
}

// TestRunSendsSound checks that the test tone reaches the receiver as audio
// packets, and that the NoSound option sends none.
func TestRunSendsSound(t *testing.T) {
	requireFFmpeg(t)
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileModern, airplay.ReceiverAuthNone, "")
	audio := func(s airplay.ReceiverStats) uint64 { return s.AudioPackets }
	receiver.runSession(t, "session with sound", 0, audio)
	if len(receiver.notices) != 0 {
		t.Errorf("session with sound gave notices: %v", receiver.notices)
	}

	receiver.opts.NoSound = true
	before := receiver.server.Stats().AudioPackets
	receiver.runSession(t, "session without sound", 0, func(s airplay.ReceiverStats) uint64 { return s.VideoPackets })
	if after := receiver.server.Stats().AudioPackets; after != before {
		t.Errorf("session without sound sent %d audio packets", after-before)
	}
}

// TestRunHighResolution streams HEVC at 3840 x 2160 with the x265 software
// encoder. On Windows a hardware encoder does this work.
func TestRunHighResolution(t *testing.T) {
	requireFFmpeg(t)
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx265") {
		t.Skip("FFmpeg has no libx265 encoder")
	}
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileModern, airplay.ReceiverAuthNone, "")
	receiver.opts.HighResolution = true
	receiver.opts.NoSound = true
	receiver.runSession(t, "4K session", 0, func(s airplay.ReceiverStats) uint64 { return s.VideoPackets })
	t.Logf("stream: %s, notices: %v", receiver.detail, receiver.notices)
	if !strings.HasSuffix(receiver.detail, "HEVC") {
		t.Errorf("4K session streams %q, want HEVC (notices: %v)", receiver.detail, receiver.notices)
	}
}

// TestRunHighResolutionFallsBack uses a receiver that cannot do HEVC. The
// session must start again with H.264 and tell the user.
func TestRunHighResolutionFallsBack(t *testing.T) {
	requireFFmpeg(t)
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileAppleTV3, airplay.ReceiverAuthNone, "")
	receiver.opts.HighResolution = true
	receiver.runSession(t, "fallback session", 0, func(s airplay.ReceiverStats) uint64 { return s.VideoDecrypted })
	if !strings.HasSuffix(receiver.detail, "H264") {
		t.Errorf("fallback session streams %q, want H264", receiver.detail)
	}
	if len(receiver.notices) == 0 || !strings.Contains(receiver.notices[0], "4K is not available") {
		t.Errorf("fallback session notices = %v, want a 4K notice", receiver.notices)
	}
}

func TestScanFindsReceiverWithoutMDNS(t *testing.T) {
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileModern, airplay.ReceiverAuthNone, "")
	// A listener that is not an AirPlay receiver must not be in the result.
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	go func() {
		for {
			conn, err := other.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("hello\r\n"))
			_ = conn.Close()
		}
	}()

	found := scanAddresses(context.Background(), []string{"127.0.0.1"}, receiver.device.Port, time.Second)
	if len(found) != 1 || found[0].Name == "" || found[0].Model != "AppleTV14,1" || found[0].IP != "127.0.0.1" {
		t.Fatalf("scan of the receiver port = %+v, want one Apple TV with a name", found)
	}
	if got := scanAddresses(context.Background(), []string{"127.0.0.1"}, other.Addr().(*net.TCPAddr).Port, time.Second); len(got) != 0 {
		t.Errorf("scan of a port without AirPlay = %+v, want nothing", got)
	}

	for _, address := range localScanAddresses() {
		if ip := net.ParseIP(address); ip == nil || !ip.IsPrivate() {
			t.Fatalf("scan list has the address %q, which is not in a private network", address)
		}
	}
}

func TestSortDevicesPutsAppleTVFirst(t *testing.T) {
	devices := []airplay.AirPlayDevice{
		{Name: "Kitchen speaker", Model: "AudioAccessory5,1"},
		{Name: "living room", Model: "AppleTV14,1"},
		{Name: "Bedroom", Model: "AppleTV6,2"},
	}
	sortDevices(devices)
	if devices[0].Name != "Bedroom" || devices[1].Name != "living room" || devices[2].Name != "Kitchen speaker" {
		t.Errorf("order = %s, %s, %s", devices[0].Name, devices[1].Name, devices[2].Name)
	}
}

// TestRunVideoDecryptsOnLegacyReceiver uses the receiver profile that
// decrypts each frame and checks its structure. It proves that the H.264
// stream from FFmpeg is framed and encrypted correctly.
func TestRunVideoDecryptsOnLegacyReceiver(t *testing.T) {
	requireFFmpeg(t)
	receiver := startLoopbackReceiver(t, airplay.ReceiverProfileAppleTV3, airplay.ReceiverAuthNone, "")
	receiver.runSession(t, "legacy session", 0, func(s airplay.ReceiverStats) uint64 { return s.VideoDecrypted })
}

func TestRunRejectsUnknownEncoder(t *testing.T) {
	err := Run(context.Background(), airplay.AirPlayDevice{IP: "127.0.0.1", Port: 1}, Options{Encoder: "bogus"}, UI{})
	if err == nil || !strings.Contains(err.Error(), "unknown encoder") {
		t.Fatalf("Run with an unknown encoder = %v, want an encoder error", err)
	}
}
