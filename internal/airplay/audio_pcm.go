package airplay

// PCM helpers for audio capture without GStreamer. This file is part of
// winairplay and is not in doubletake.
//
// The AirPlay audio encoders need 16-bit stereo PCM at 44100 Hz. Windows
// gives the sound of the PC in the format of the audio device, usually
// 32-bit float at 48000 Hz. The functions in this file convert it.

import (
	"encoding/binary"
	"io"
	"math"
	"sync"
)

// pcmSourceFormat describes interleaved PCM as an audio device gives it.
type pcmSourceFormat struct {
	sampleRate     int
	channels       int
	bytesPerSample int // 2, 3 or 4
	float          bool
}

func (f pcmSourceFormat) blockAlign() int { return f.channels * f.bytesPerSample }

func (f pcmSourceFormat) valid() bool {
	if f.sampleRate < 8000 || f.sampleRate > 384000 || f.channels < 1 || f.channels > 32 {
		return false
	}
	if f.float {
		return f.bytesPerSample == 4
	}
	return f.bytesPerSample >= 2 && f.bytesPerSample <= 4
}

// sample returns one sample in the range -1 to 1.
func (f pcmSourceFormat) sample(b []byte) float32 {
	switch {
	case f.float:
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	case f.bytesPerSample == 2:
		return float32(int16(binary.LittleEndian.Uint16(b))) / 32768
	case f.bytesPerSample == 3:
		v := int32(b[0]) | int32(b[1])<<8 | int32(int8(b[2]))<<16
		return float32(v) / 8388608
	default:
		return float32(int32(binary.LittleEndian.Uint32(b))) / 2147483648
	}
}

// toStereo converts device PCM to interleaved stereo float samples and
// appends them to dst. Mono is copied to the two channels. For surround
// layouts (front left, front right, center, LFE, surround...), the center and
// the surround channels are mixed into left and right.
func (f pcmSourceFormat) toStereo(dst []float32, src []byte) []float32 {
	align := f.blockAlign()
	bps := f.bytesPerSample
	for offset := 0; offset+align <= len(src); offset += align {
		frame := src[offset : offset+align]
		at := func(channel int) float32 { return f.sample(frame[channel*bps:]) }
		var left, right float32
		switch {
		case f.channels == 1:
			left = at(0)
			right = left
		case f.channels < 6:
			left, right = at(0), at(1)
		default:
			center := 0.707 * at(2)
			left = at(0) + center + 0.707*at(4)
			right = at(1) + center + 0.707*at(5)
			if f.channels >= 8 {
				left += 0.707 * at(6)
				right += 0.707 * at(7)
			}
			left *= 0.6
			right *= 0.6
		}
		dst = append(dst, left, right)
	}
	return dst
}

// stereoResampler converts interleaved stereo float samples from one sample
// rate to 16-bit samples at 44100 Hz. It is a polyphase filter with a
// windowed sinc function, which keeps the sound clear and removes the
// frequencies that the lower rate cannot carry.
type stereoResampler struct {
	up, down int         // output rate / input rate = up / down
	half     int         // filter half width in input frames
	table    [][]float32 // one filter for each phase
	input    []float32   // stereo frames not yet fully used
	index    int         // input frame at the filter center
	phase    int         // position between frames, in units of 1/up
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func newStereoResampler(sourceRate int) *stereoResampler {
	divisor := gcd(audioSampleRate, sourceRate)
	r := &stereoResampler{up: audioSampleRate / divisor, down: sourceRate / divisor}
	if r.up == r.down {
		return r
	}
	cutoff := 0.94
	widen := 1
	if r.down > r.up {
		cutoff *= float64(r.up) / float64(r.down)
		widen = (r.down + r.up - 1) / r.up
	}
	r.half = 16 * widen
	r.table = make([][]float32, r.up)
	for phase := range r.table {
		filter := make([]float32, 2*r.half)
		var sum float64
		for tap := range filter {
			x := float64(tap-r.half+1) - float64(phase)/float64(r.up)
			value := cutoff
			if x != 0 {
				value = math.Sin(math.Pi*cutoff*x) / (math.Pi * x)
			}
			// Blackman window over the filter width.
			w := x / float64(r.half)
			if w <= -1 || w >= 1 {
				value = 0
			} else {
				value *= 0.42 + 0.5*math.Cos(math.Pi*w) + 0.08*math.Cos(2*math.Pi*w)
			}
			filter[tap] = float32(value)
			sum += value
		}
		for tap := range filter {
			filter[tap] = float32(float64(filter[tap]) / sum)
		}
		r.table[phase] = filter
	}
	// The first output sample is at input frame 0. Start with silence before it.
	r.input = make([]float32, 2*(r.half-1))
	r.index = r.half - 1
	return r
}

func clampInt16(v float32) int16 {
	v *= 32767
	switch {
	case v >= 32767:
		return 32767
	case v <= -32768:
		return -32768
	case v >= 0:
		return int16(v + 0.5)
	default:
		return int16(v - 0.5)
	}
}

// process appends the converted samples (16-bit little-endian stereo) to dst.
func (r *stereoResampler) process(dst []byte, stereo []float32) []byte {
	if r.up == r.down {
		for _, v := range stereo {
			dst = binary.LittleEndian.AppendUint16(dst, uint16(clampInt16(v)))
		}
		return dst
	}
	r.input = append(r.input, stereo...)
	frames := len(r.input) / 2
	for r.index+r.half < frames {
		filter := r.table[r.phase]
		base := 2 * (r.index - r.half + 1)
		var left, right float32
		for tap, weight := range filter {
			left += r.input[base+2*tap] * weight
			right += r.input[base+2*tap+1] * weight
		}
		dst = binary.LittleEndian.AppendUint16(dst, uint16(clampInt16(left)))
		dst = binary.LittleEndian.AppendUint16(dst, uint16(clampInt16(right)))
		r.phase += r.down
		r.index += r.phase / r.up
		r.phase %= r.up
	}
	// Discard the frames that no later output sample needs.
	if discard := r.index - r.half + 1; discard > 0 {
		r.input = append(r.input[:0], r.input[2*discard:]...)
		r.index -= discard
	}
	return dst
}

// pcmQueue carries PCM from the capture thread to the encoder. When the
// reader is too slow, Write discards the oldest sound, so the sound can
// never stay behind the picture.
type pcmQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	limit  int
	closed bool
	err    error
}

func newPCMQueue(limitBytes int) *pcmQueue {
	q := &pcmQueue{limit: limitBytes &^ (audioBytesPerSampleFrame - 1)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *pcmQueue) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, io.ErrClosedPipe
	}
	q.data = append(q.data, p...)
	if excess := len(q.data) - q.limit; excess > 0 {
		excess = (excess + audioBytesPerSampleFrame - 1) &^ (audioBytesPerSampleFrame - 1)
		q.data = append(q.data[:0], q.data[excess:]...)
	}
	q.cond.Broadcast()
	return len(p), nil
}

func (q *pcmQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.data) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.data) == 0 {
		if q.err != nil {
			return 0, q.err
		}
		return 0, io.EOF
	}
	n := copy(p, q.data)
	q.data = append(q.data[:0], q.data[n:]...)
	return n, nil
}

// closeWithError ends the stream. A blocked Read returns err, or io.EOF.
func (q *pcmQueue) closeWithError(err error) {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.err = err
	}
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *pcmQueue) Close() error {
	q.closeWithError(nil)
	return nil
}
