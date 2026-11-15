//go:build windows && (amd64 || arm64)

package airplay

import (
	"context"
	"testing"
	"time"
)

// TestLoopbackCaptureOnThisComputer records the PC sound for a short time.
// A computer without a playback device (a build server) skips the test, but
// the calls into the Windows audio API are done in each case.
func TestLoopbackCaptureOnThisComputer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newPCMQueue(1 << 20)
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() { done <- runLoopbackCapture(ctx, queue, ready) }()

	select {
	case err := <-ready:
		if err != nil {
			<-done
			t.Skipf("no usable playback device: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the audio device did not open")
	}
	time.Sleep(1500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("capture ended with an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not stop")
	}
	queue.mu.Lock()
	bytes := len(queue.data)
	queue.mu.Unlock()
	seconds := float64(bytes) / (audioSampleRate * audioBytesPerSampleFrame)
	t.Logf("captured %.2f seconds of sound in 1.5 seconds", seconds)
	// Silence is filled in, so the stream must be continuous.
	if seconds < 1.2 || seconds > 1.7 {
		t.Errorf("captured %.2f seconds of sound, want approximately 1.5", seconds)
	}
}
