package mirror

import (
	"context"
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
	var prompts atomic.Int32
	streaming := make(chan struct{}, 1)
	ui := UI{
		AskCredential: func(Prompt, string) (string, bool) {
			prompts.Add(1)
			return r.pin, true
		},
		Streaming: func(string) { streaming <- struct{}{} },
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
	receiver.runSession(t, "second session", 0, packets)
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
