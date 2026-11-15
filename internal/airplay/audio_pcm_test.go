package airplay

import (
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"
)

// sineStereo makes an interleaved stereo sine wave with amplitude 0.5.
func sineStereo(rate int, frequency float64, frames int) []float32 {
	out := make([]float32, 2*frames)
	for i := 0; i < frames; i++ {
		v := float32(0.5 * math.Sin(2*math.Pi*frequency*float64(i)/float64(rate)))
		out[2*i], out[2*i+1] = v, v
	}
	return out
}

// analyze returns the RMS level (1.0 = full scale) and the frequency of the
// left channel, from its zero crossings. It ignores the first 2000 frames.
func analyze(pcm []byte) (rms, frequency float64) {
	const skip = 2000
	frames := len(pcm) / 4
	var sum float64
	crossings, previous := 0, 0.0
	for i := skip; i < frames; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[4*i:]))) / 32768
		sum += v * v
		if previous < 0 && v >= 0 {
			crossings++
		}
		previous = v
	}
	n := float64(frames - skip)
	return math.Sqrt(sum / n), float64(crossings) * audioSampleRate / n
}

func TestStereoResamplerKeepsToneAndLevel(t *testing.T) {
	for _, rate := range []int{48000, 96000, 32000, 44100, 192000} {
		resampler := newStereoResampler(rate)
		pcm := resampler.process(nil, sineStereo(rate, 1000, rate))
		if got, want := len(pcm)/4, audioSampleRate; got < want-200 || got > want {
			t.Errorf("%d Hz: one second gave %d frames, want approximately %d", rate, got, want)
		}
		rms, frequency := analyze(pcm)
		if math.Abs(rms-0.5/math.Sqrt2) > 0.01 {
			t.Errorf("%d Hz: RMS level %.4f, want %.4f", rate, rms, 0.5/math.Sqrt2)
		}
		if math.Abs(frequency-1000) > 5 {
			t.Errorf("%d Hz: tone frequency %.1f Hz, want 1000 Hz", rate, frequency)
		}
	}
}

func TestStereoResamplerRemovesFrequenciesAboveTheLimit(t *testing.T) {
	// 23 kHz is above half of 44100 Hz. Without a filter it would come out
	// as a false tone.
	resampler := newStereoResampler(48000)
	rms, _ := analyze(resampler.process(nil, sineStereo(48000, 23000, 48000)))
	if rms > 0.01 {
		t.Errorf("23 kHz tone has RMS level %.4f after conversion, want less than 0.01", rms)
	}
}

func TestStereoResamplerGivesTheSameResultForSmallBlocks(t *testing.T) {
	input := sineStereo(48000, 440, 9600)
	whole := newStereoResampler(48000).process(nil, input)
	blocks := newStereoResampler(48000)
	var pieces []byte
	for offset := 0; offset < len(input); offset += 2 * 137 {
		end := min(offset+2*137, len(input))
		pieces = blocks.process(pieces, input[offset:end])
	}
	if len(whole) != len(pieces) {
		t.Fatalf("block processing gave %d bytes, one call gave %d", len(pieces), len(whole))
	}
	for i := range whole {
		if whole[i] != pieces[i] {
			t.Fatalf("block processing differs at byte %d", i)
		}
	}
}

func TestPCMSourceFormatToStereo(t *testing.T) {
	float := pcmSourceFormat{sampleRate: 48000, channels: 2, bytesPerSample: 4, float: true}
	src := binary.LittleEndian.AppendUint32(nil, math.Float32bits(0.25))
	src = binary.LittleEndian.AppendUint32(src, math.Float32bits(-0.5))
	if got := float.toStereo(nil, src); len(got) != 2 || got[0] != 0.25 || got[1] != -0.5 {
		t.Errorf("float stereo = %v, want [0.25 -0.5]", got)
	}

	mono16 := pcmSourceFormat{sampleRate: 44100, channels: 1, bytesPerSample: 2}
	if got := mono16.toStereo(nil, []byte{0x00, 0x40}); len(got) != 2 || got[0] != 0.5 || got[1] != 0.5 {
		t.Errorf("16-bit mono = %v, want [0.5 0.5]", got)
	}

	pcm24 := pcmSourceFormat{sampleRate: 48000, channels: 2, bytesPerSample: 3}
	if got := pcm24.toStereo(nil, []byte{0, 0, 0x40, 0, 0, 0xC0}); len(got) != 2 || got[0] != 0.5 || got[1] != -0.5 {
		t.Errorf("24-bit stereo = %v, want [0.5 -0.5]", got)
	}

	// 5.1: sound only in the center channel must come out of left and right.
	surround := pcmSourceFormat{sampleRate: 48000, channels: 6, bytesPerSample: 2}
	frame := make([]byte, 12)
	binary.LittleEndian.PutUint16(frame[4:], 0x4000)
	got := surround.toStereo(nil, frame)
	if len(got) != 2 || got[0] <= 0.1 || got[0] != got[1] {
		t.Errorf("5.1 center = %v, want the same positive level on left and right", got)
	}

	for _, bad := range []pcmSourceFormat{
		{sampleRate: 48000, channels: 0, bytesPerSample: 2},
		{sampleRate: 48000, channels: 2, bytesPerSample: 2, float: true},
		{sampleRate: 100, channels: 2, bytesPerSample: 2},
	} {
		if bad.valid() {
			t.Errorf("format %+v must not be valid", bad)
		}
	}
}

func TestPCMQueueDiscardsOldSoundAndReportsTheEnd(t *testing.T) {
	queue := newPCMQueue(16)
	for i := byte(0); i < 6; i++ {
		_, _ = queue.Write([]byte{i, i, i, i})
	}
	buf := make([]byte, 64)
	n, err := queue.Read(buf)
	if err != nil || n != 16 || buf[0] != 2 || buf[15] != 5 {
		t.Fatalf("Read = %d bytes %v, %v; want the 16 newest bytes (frames 2 to 5)", n, buf[:n], err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := queue.Read(buf)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = queue.Close()
	select {
	case err := <-done:
		if err != io.EOF {
			t.Errorf("Read after Close = %v, want io.EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not release the blocked Read")
	}
	if _, err := queue.Write([]byte{1, 2, 3, 4}); err == nil {
		t.Error("Write after Close must fail")
	}
}

// simulateFrameClock runs the clock for a time with a device that gives
// deviceRate frames in each second while playing is true, in 10 ms steps.
func simulateFrameClock(t *testing.T, seconds int, deviceRate float64, playing func(step int) bool) (delivered int64, worst time.Duration) {
	t.Helper()
	const rate = 48000
	start := time.Unix(5000, 0)
	clock := newRealtimeFrameClock(rate, start)
	var deviceFrames float64
	var given int64
	for step := 1; step <= seconds*100; step++ {
		now := start.Add(time.Duration(step) * 10 * time.Millisecond)
		got := 0
		if playing(step) {
			deviceFrames += deviceRate / 100
			got = int(int64(deviceFrames) - given)
			given += int64(got)
		}
		pad, drop := clock.adjust(now, got)
		if pad < 0 || drop < 0 || drop > got {
			t.Fatalf("step %d: pad %d, drop %d for %d frames", step, pad, drop, got)
		}
		delivered += int64(got + pad - drop)
		behind := now.Sub(start) - time.Duration(delivered)*time.Second/rate
		if behind < 0 {
			behind = -behind
		}
		worst = max(worst, behind)
	}
	return delivered, worst
}

func TestRealtimeFrameClockFollowsTheTime(t *testing.T) {
	cases := []struct {
		name       string
		deviceRate float64
		playing    func(step int) bool
	}{
		{"continuous sound", 48000, func(int) bool { return true }},
		{"no sound at all", 48000, func(int) bool { return false }},
		// Sound that starts and stops each 0.7 seconds.
		{"sound with pauses", 48000, func(step int) bool { return (step/70)%2 == 0 }},
		{"device clock 0.1 percent slow", 47952, func(int) bool { return true }},
		{"device clock 0.1 percent fast", 48048, func(int) bool { return true }},
	}
	for _, tc := range cases {
		delivered, worst := simulateFrameClock(t, 600, tc.deviceRate, tc.playing)
		// In 10 minutes the sound must stay within 50 ms of real time.
		if worst > 50*time.Millisecond {
			t.Errorf("%s: sound was %v away from real time", tc.name, worst)
		}
		if want := int64(600 * 48000); delivered < want-2400 || delivered > want+2400 {
			t.Errorf("%s: %d frames in 600 seconds, want %d", tc.name, delivered, want)
		}
	}
}

func TestRealtimeFrameClockDoesNotChangeGoodSound(t *testing.T) {
	// A device with a correct clock that delivers 10 ms late gets no change.
	const rate = 44100
	start := time.Unix(100, 0)
	clock := newRealtimeFrameClock(rate, start)
	for step := 2; step <= 3000; step++ {
		now := start.Add(time.Duration(step) * 10 * time.Millisecond)
		if pad, drop := clock.adjust(now, 441); pad != 0 || drop != 0 {
			t.Fatalf("step %d: pad %d, drop %d; want no change", step, pad, drop)
		}
	}
}

func TestSetAudioOutputMode(t *testing.T) {
	defer SetAudioOutputMode(AudioOutputBoth)
	SetAudioOutputMode(AudioOutputPCOnly)
	if currentAudioOutputMode() != AudioOutputPCOnly {
		t.Error("the mode did not change")
	}
	SetAudioOutputMode(AudioOutputMode(99))
	if currentAudioOutputMode() != AudioOutputBoth {
		t.Error("a mode that is not valid must select the default")
	}
}

func TestPCMQueueReadFrameGivesPositionAndTime(t *testing.T) {
	queue := newPCMQueue(4 * 100)
	epoch := time.Unix(2000, 0)
	queue.setEpoch(epoch)
	reader := pcmQueueFrameReader{queue}

	_, _ = queue.Write(make([]byte, 4*30))
	frame := make([]byte, 4*10)
	for want := int64(0); want < 30; want += 10 {
		position, err := reader.ReadPCMFramePosition(frame)
		if err != nil || !position.HasSourceRTP || int64(position.SourceRTP) != want {
			t.Fatalf("position = %+v, %v; want sample %d", position, err, want)
		}
		if wantPTS := epoch.Add(time.Duration(want) * time.Second / audioSampleRate); !position.PTS.Equal(wantPTS) {
			t.Errorf("sample %d has time %v, want %v", want, position.PTS, wantPTS)
		}
	}

	// 150 frames into a queue for 100: the 50 oldest are discarded, and the
	// position of the next frame shows the gap.
	_, _ = queue.Write(make([]byte, 4*150))
	position, err := reader.ReadPCMFramePosition(frame)
	if err != nil || position.SourceRTP != 30+50 {
		t.Fatalf("position after an overflow = %+v, %v; want sample 80", position, err)
	}

	// A frame that is not complete at the end of the stream is not returned.
	queue = newPCMQueue(400)
	_, _ = queue.Write(make([]byte, 4*5))
	_ = queue.Close()
	if _, err := (pcmQueueFrameReader{queue}).ReadPCMFrame(frame); err != io.EOF {
		t.Errorf("read of a part frame after Close = %v, want io.EOF", err)
	}
}
