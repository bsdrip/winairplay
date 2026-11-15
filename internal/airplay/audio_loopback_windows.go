//go:build windows && (amd64 || arm64)

package airplay

// WASAPI loopback capture: records the sound that the PC plays on its
// default output device. This file is part of winairplay and is not in
// doubletake. It calls the COM interfaces of the Windows audio API directly,
// so the program needs no cgo.

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	clsidMMDeviceEnumerator = windows.GUID{Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C, Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E}}
	iidIMMDeviceEnumerator  = windows.GUID{Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35, Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6}}
	iidIAudioClient         = windows.GUID{Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32, Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2}}
	iidIAudioCaptureClient  = windows.GUID{Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0, Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17}}

	procCoCreateInstance = windows.NewLazySystemDLL("ole32.dll").NewProc("CoCreateInstance")
)

const (
	clsctxAll                  = 23
	coinitMultithreaded        = 0
	audclntStreamFlagsLoopback = 0x00020000
	audclntBufferFlagsSilent   = 0x2
	waveFormatPCM              = 1
	waveFormatIEEEFloat        = 3
	waveFormatExtensible       = 0xFFFE
	loopbackBufferHNS          = 200 * 10000 // 200 ms in units of 100 ns
	loopbackPollInterval       = 10 * time.Millisecond
	loopbackSilenceAfter       = 40 * time.Millisecond
	loopbackSilenceMargin      = 20 * time.Millisecond
	loopbackReopenInterval     = time.Second

	// Indexes into the COM method tables. The first three are IUnknown.
	comRelease                        = 2
	enumeratorGetDefaultAudioEndpoint = 4
	deviceActivate                    = 3
	audioClientInitialize             = 3
	audioClientGetMixFormat           = 8
	audioClientStart                  = 10
	audioClientStop                   = 11
	audioClientGetService             = 14
	captureClientGetBuffer            = 3
	captureClientReleaseBuffer        = 4
	captureClientGetNextPacketSize    = 5
)

// comCall calls method number index of a COM object. It returns the HRESULT.
func comCall(object unsafe.Pointer, index int, args ...uintptr) int32 {
	table := *(*unsafe.Pointer)(object)
	method := *(*uintptr)(unsafe.Add(table, uintptr(index)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(object))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(method, all...)
	return int32(r)
}

func comRelease1(object unsafe.Pointer) {
	if object != nil {
		comCall(object, comRelease)
	}
}

func hresult(operation string, hr int32) error {
	return fmt.Errorf("%s failed (HRESULT 0x%08X)", operation, uint32(hr))
}

// loopbackStream is one open capture stream on the default output device.
type loopbackStream struct {
	client  unsafe.Pointer // IAudioClient
	capture unsafe.Pointer // IAudioCaptureClient
	format  pcmSourceFormat
	started bool
}

func (s *loopbackStream) close() {
	if s.started {
		comCall(s.client, audioClientStop)
	}
	comRelease1(s.capture)
	comRelease1(s.client)
	s.capture, s.client = nil, nil
}

// parseWaveFormat reads a WAVEFORMATEX or WAVEFORMATEXTENSIBLE structure.
func parseWaveFormat(p unsafe.Pointer) (pcmSourceFormat, error) {
	header := unsafe.Slice((*byte)(p), 18)
	tag := binary.LittleEndian.Uint16(header[0:])
	channels := int(binary.LittleEndian.Uint16(header[2:]))
	rate := int(binary.LittleEndian.Uint32(header[4:]))
	blockAlign := int(binary.LittleEndian.Uint16(header[12:]))
	extraSize := int(binary.LittleEndian.Uint16(header[16:]))
	if tag == waveFormatExtensible && extraSize >= 22 {
		extended := unsafe.Slice((*byte)(p), 40)
		// The first field of the SubFormat GUID is the basic format tag.
		tag = uint16(binary.LittleEndian.Uint32(extended[24:]))
	}
	format := pcmSourceFormat{sampleRate: rate, channels: channels, float: tag == waveFormatIEEEFloat}
	if channels > 0 {
		format.bytesPerSample = blockAlign / channels
	}
	if (tag != waveFormatPCM && tag != waveFormatIEEEFloat) || !format.valid() {
		return format, fmt.Errorf("audio device format is not supported (tag %d, %d channels, %d Hz, block %d)", tag, channels, rate, blockAlign)
	}
	return format, nil
}

func openLoopbackStream() (*loopbackStream, error) {
	var enumerator unsafe.Pointer
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)), 0, clsctxAll,
		uintptr(unsafe.Pointer(&iidIMMDeviceEnumerator)), uintptr(unsafe.Pointer(&enumerator)))
	if hr := int32(r); hr < 0 || enumerator == nil {
		return nil, hresult("create the audio device enumerator", hr)
	}
	defer comRelease1(enumerator)

	var device unsafe.Pointer
	// eRender (0), eConsole (0): the default playback device.
	if hr := comCall(enumerator, enumeratorGetDefaultAudioEndpoint, 0, 0, uintptr(unsafe.Pointer(&device))); hr < 0 || device == nil {
		return nil, hresult("find the default playback device", hr)
	}
	defer comRelease1(device)

	stream := &loopbackStream{}
	if hr := comCall(device, deviceActivate, uintptr(unsafe.Pointer(&iidIAudioClient)), clsctxAll, 0, uintptr(unsafe.Pointer(&stream.client))); hr < 0 || stream.client == nil {
		return nil, hresult("open the playback device", hr)
	}
	var waveFormat unsafe.Pointer
	if hr := comCall(stream.client, audioClientGetMixFormat, uintptr(unsafe.Pointer(&waveFormat))); hr < 0 || waveFormat == nil {
		stream.close()
		return nil, hresult("read the device format", hr)
	}
	defer windows.CoTaskMemFree(waveFormat)
	format, err := parseWaveFormat(waveFormat)
	if err != nil {
		stream.close()
		return nil, err
	}
	stream.format = format
	// Shared mode (0) with the loopback flag records what the device plays.
	if hr := comCall(stream.client, audioClientInitialize, 0, audclntStreamFlagsLoopback, loopbackBufferHNS, 0, uintptr(waveFormat), 0); hr < 0 {
		stream.close()
		return nil, hresult("initialize loopback capture", hr)
	}
	if hr := comCall(stream.client, audioClientGetService, uintptr(unsafe.Pointer(&iidIAudioCaptureClient)), uintptr(unsafe.Pointer(&stream.capture))); hr < 0 || stream.capture == nil {
		stream.close()
		return nil, hresult("get the capture service", hr)
	}
	if hr := comCall(stream.client, audioClientStart); hr < 0 {
		stream.close()
		return nil, hresult("start loopback capture", hr)
	}
	stream.started = true
	return stream, nil
}

// readPackets appends all waiting sound to stereo. It returns the number of
// device frames that it read.
func (s *loopbackStream) readPackets(stereo []float32) ([]float32, int, error) {
	total := 0
	for {
		var packetFrames uint32
		if hr := comCall(s.capture, captureClientGetNextPacketSize, uintptr(unsafe.Pointer(&packetFrames))); hr < 0 {
			return stereo, total, hresult("read the packet size", hr)
		}
		if packetFrames == 0 {
			return stereo, total, nil
		}
		var data unsafe.Pointer
		var frames, flags uint32
		hr := comCall(s.capture, captureClientGetBuffer, uintptr(unsafe.Pointer(&data)),
			uintptr(unsafe.Pointer(&frames)), uintptr(unsafe.Pointer(&flags)), 0, 0)
		if hr < 0 {
			return stereo, total, hresult("read the capture buffer", hr)
		}
		if frames > 0 {
			if flags&audclntBufferFlagsSilent != 0 || data == nil {
				stereo = append(stereo, make([]float32, 2*int(frames))...)
			} else {
				stereo = s.format.toStereo(stereo, unsafe.Slice((*byte)(data), int(frames)*s.format.blockAlign()))
			}
			total += int(frames)
		}
		if hr := comCall(s.capture, captureClientReleaseBuffer, uintptr(frames)); hr < 0 {
			return stereo, total, hresult("release the capture buffer", hr)
		}
	}
}

// runLoopbackCapture writes the sound of the PC to out as 16-bit stereo PCM
// at 44100 Hz until ctx is cancelled. It sends the result of the first open
// to ready. Windows gives no data while no program plays sound, so this
// function writes silence for those times: the receiver needs a continuous
// stream. When the default device changes, it opens the new device.
func runLoopbackCapture(ctx context.Context, out io.Writer, ready chan<- error) error {
	// COM objects of this function stay on one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE (1) means that COM is already initialized on this thread, which
	// is not an error. RPC_E_CHANGED_MODE means that the thread uses the other
	// COM mode; the audio interfaces operate in the two modes.
	const sFalse, rpcEChangedMode = syscall.Errno(1), syscall.Errno(0x80010106)
	switch err := windows.CoInitializeEx(0, coinitMultithreaded); err {
	case nil, sFalse:
		defer windows.CoUninitialize()
	case rpcEChangedMode:
	default:
		ready <- fmt.Errorf("initialize COM: %w", err)
		return err
	}

	stream, err := openLoopbackStream()
	ready <- err
	if err != nil {
		return err
	}
	defer func() {
		if stream != nil {
			stream.close()
		}
	}()
	log.Printf("[AUDIO] capturing the PC sound: %d Hz, %d channels", stream.format.sampleRate, stream.format.channels)

	resampler := newStereoResampler(stream.format.sampleRate)
	sourceRate := stream.format.sampleRate
	var stereo []float32
	var pcm []byte
	lastData := time.Now()
	lastOpenAttempt := time.Now()
	ticker := time.NewTicker(loopbackPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		stereo = stereo[:0]
		frames := 0
		if stream != nil {
			var readErr error
			stereo, frames, readErr = stream.readPackets(stereo)
			if readErr != nil {
				// The usual cause: the default device changed or was removed.
				log.Printf("[AUDIO] %v; opening the playback device again", readErr)
				stream.close()
				stream = nil
				lastOpenAttempt = time.Now()
			}
		} else if time.Since(lastOpenAttempt) >= loopbackReopenInterval {
			lastOpenAttempt = time.Now()
			if reopened, openErr := openLoopbackStream(); openErr == nil {
				stream = reopened
				if stream.format.sampleRate != sourceRate {
					sourceRate = stream.format.sampleRate
					resampler = newStereoResampler(sourceRate)
				}
				log.Printf("[AUDIO] capturing the PC sound again: %d Hz, %d channels", sourceRate, stream.format.channels)
			}
		}
		now := time.Now()
		if frames > 0 {
			lastData = now
		} else if gap := now.Sub(lastData); gap >= loopbackSilenceAfter {
			// No sound came: write silence up to a short time before now.
			fill := gap - loopbackSilenceMargin
			silentFrames := int(fill * time.Duration(sourceRate) / time.Second)
			stereo = append(stereo, make([]float32, 2*silentFrames)...)
			lastData = now.Add(-loopbackSilenceMargin)
		}
		if len(stereo) == 0 {
			continue
		}
		pcm = resampler.process(pcm[:0], stereo)
		if len(pcm) > 0 {
			if _, err := out.Write(pcm); err != nil {
				return nil // the reader closed the stream
			}
		}
	}
}
