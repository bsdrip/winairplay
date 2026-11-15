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
