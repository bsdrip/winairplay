package airplay

// Audio capture without GStreamer. This file is part of winairplay and is
// not in doubletake.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// audioQueueLimit is the most PCM that can wait for the encoder.
const audioQueueLimit = audioSampleRate * audioBytesPerSampleFrame / 4 // 250 ms

// startPlatformAudioCapture is StartAudioCapture for the FFmpeg backend. On
// Windows it captures the sound that the PC plays (WASAPI loopback). The
// test tone comes from FFmpeg and operates on all systems.
func startPlatformAudioCapture(ctx context.Context, cancel context.CancelFunc, testTone bool, codec AudioCodec) (*AudioCapture, error) {
	ac := &AudioCapture{cancel: cancel, waitCh: make(chan struct{}), codec: codec}
	if codec == AudioCodecALAC {
		ac.alac = &alacEncoder{}
	} else {
		eld, err := newELDEncoder()
		if err != nil {
			cancel()
			return nil, err
		}
		ac.eld = eld
	}
	fail := func(err error) (*AudioCapture, error) {
		if ac.eld != nil {
			ac.eld.Close()
		}
		cancel()
		return nil, err
	}

	if testTone {
		path, err := FindFFmpeg()
		if err != nil {
			return fail(err)
		}
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-re",
			"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d", audioSampleRate),
			"-ac", fmt.Sprint(audioChannels), "-f", "s16le", "-flush_packets", "1", "pipe:1"}
		dbg("[AUDIO] test tone: %s %s", path, strings.Join(args, " "))
		cmd := exec.CommandContext(ctx, path, args...)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fail(fmt.Errorf("ffmpeg stdout pipe: %w", err))
		}
		stderr, _ := cmd.StderrPipe()
		waitResult, err := startGStreamerCommand(cmd)
		if err != nil {
			return fail(fmt.Errorf("start the audio test tone: %w", err))
		}
		go logStderr("AUDIO-FFMPEG", stderr)
		// Send the tone through the same queue as real sound, so that the
		// tests use the same time stamp path.
		queue := newPCMQueue(audioQueueLimit)
		go func() {
			buf := make([]byte, 4096)
			first := true
			for {
				n, err := stdout.Read(buf)
				if n > 0 {
					if first {
						queue.setEpoch(time.Now())
						first = false
					}
					_, _ = queue.Write(buf[:n])
				}
				if err != nil {
					queue.closeWithError(nil)
					return
				}
			}
		}()
		ac.gstCmd = cmd
		ac.pcmPipe = queue
		ac.pcmFrames = pcmQueueFrameReader{queue}
		go func() {
			ac.waitErr = <-waitResult
			_ = stdout.Close()
			close(ac.waitCh)
		}()
		return ac, nil
	}

	queue := newPCMQueue(audioQueueLimit)
	ready := make(chan error, 1)
	go func() {
		err := runLoopbackCapture(ctx, queue, ready)
		if ctx.Err() != nil {
			err = nil
		}
		ac.waitErr = err
		queue.closeWithError(err)
		close(ac.waitCh)
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-ac.waitCh
			return fail(fmt.Errorf("capture the sound of the PC: %w", err))
		}
	case <-time.After(5 * time.Second):
		cancel()
		return fail(fmt.Errorf("capture the sound of the PC: the audio device did not start"))
	}
	ac.pcmPipe = queue
	ac.pcmFrames = pcmQueueFrameReader{queue}
	return ac, nil
}
