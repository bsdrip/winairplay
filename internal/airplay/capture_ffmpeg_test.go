package airplay

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

const sampleEncoderList = `Encoders:
 V..... = Video
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V....D h264_mf              H264 via MediaFoundation (codec h264)
`

func TestHasListedName(t *testing.T) {
	for name, want := range map[string]bool{
		"libx264":    true,
		"h264_nvenc": true,
		"h264_qsv":   false,
		"H.264":      false, // only the name column counts
		"x264":       false,
	} {
		if got := hasListedName(sampleEncoderList, name); got != want {
			t.Errorf("hasListedName(%q) = %t, want %t", name, got, want)
		}
	}
}

func TestSelectFFmpegEncoder(t *testing.T) {
	caps := &ffmpegCapabilities{encoders: map[string]bool{"libx264": true, "h264_nvenc": true, "h264_mf": true}}
	tests := []struct {
		hwaccel string
		want    string
		wantErr bool
	}{
		{"", "libx264", false},
		{"auto", "libx264", false},
		{"none", "libx264", false},
		{"nvenc", "h264_nvenc", false},
		{"mf", "h264_mf", false},
		{"qsv", "", true},   // not in this build
		{"vaapi", "", true}, // GStreamer only
	}
	for _, tt := range tests {
		got, err := selectFFmpegEncoder(caps, tt.hwaccel, VideoCodecH264, nil)
		if (err != nil) != tt.wantErr || got.name != tt.want {
			t.Errorf("selectFFmpegEncoder(%q) = %q, %v; want %q, error %t", tt.hwaccel, got.name, err, tt.want, tt.wantErr)
		}
	}

	// Without x264, automatic selection uses the next software encoder.
	caps = &ffmpegCapabilities{encoders: map[string]bool{"h264_nvenc": true, "h264_mf": true}}
	if got, err := selectFFmpegEncoder(caps, "auto", VideoCodecH264, nil); err != nil || got.name != "h264_mf" {
		t.Errorf("automatic selection without x264 = %q, %v; want h264_mf", got.name, err)
	}
	if _, err := selectFFmpegEncoder(&ffmpegCapabilities{encoders: map[string]bool{}}, "auto", VideoCodecH264, nil); err == nil {
		t.Error("automatic selection with no encoder must fail")
	}
}

func TestFFmpegScaleFilter(t *testing.T) {
	fitted := ffmpegScaleFilter(1921, 1081, "yuv420p")
	for _, want := range []string{"scale=1920:1080:", "force_original_aspect_ratio=decrease", "pad=1920:1080:", "format=yuv420p"} {
		if !strings.Contains(fitted, want) {
			t.Errorf("fitted filter %q does not contain %q", fitted, want)
		}
	}
	native := ffmpegScaleFilter(0, 0, "nv12")
	if strings.Contains(native, "pad=") || !strings.Contains(native, "trunc(iw/2)*2") || !strings.Contains(native, "format=nv12") {
		t.Errorf("native filter %q must only make the size even", native)
	}
}

func TestFFmpegCaptureArgs(t *testing.T) {
	cfg := CaptureConfig{FPS: 60, Bitrate: 6000, MaxWidth: 1920, MaxHeight: 1080, ShowCursor: true, Display: 1}
	x264 := ffmpegEncoder{name: "libx264", pixFmt: "yuv420p", codec: VideoCodecH264}

	args := strings.Join(ffmpegCaptureArgs(cfg, x264, "ddagrab"), " ")
	for _, want := range []string{
		"ddagrab=output_idx=1:framerate=60:draw_mouse=1,hwdownload,format=bgra",
		"-c:v libx264", "-tune zerolatency", "-b:v 6000k", "-g 120", "-bf 0",
		"repeat-headers=1", "-f h264 pipe:1",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("ddagrab arguments do not contain %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "dump_extra") {
		t.Errorf("x264 repeats its headers and needs no dump_extra filter:\n%s", args)
	}

	cfg.ShowCursor = false
	args = strings.Join(ffmpegCaptureArgs(cfg, ffmpegEncoder{name: "h264_nvenc", pixFmt: "nv12", codec: VideoCodecH264}, "gdigrab"), " ")
	for _, want := range []string{"-f gdigrab -framerate 60 -draw_mouse 0 -i desktop", "-c:v h264_nvenc", "format=nv12", "dump_extra=freq=keyframe"} {
		if !strings.Contains(args, want) {
			t.Errorf("gdigrab arguments do not contain %q:\n%s", want, args)
		}
	}

	args = strings.Join(ffmpegCaptureArgs(CaptureConfig{Bitrate: 3000}, x264, "test"), " ")
	if !strings.Contains(args, "testsrc2=size=1920x1080:rate=30") {
		t.Errorf("test arguments must use the test source at 30 fps:\n%s", args)
	}
}

func TestSelectFFmpegHEVCEncoder(t *testing.T) {
	caps := &ffmpegCapabilities{encoders: map[string]bool{"hevc_nvenc": true, "hevc_qsv": true, "hevc_amf": true, "libx265": true}}
	onlyQSV := func(name string) bool { return name == "hevc_qsv" }
	if got, err := selectFFmpegEncoder(caps, "auto", VideoCodecHEVC, onlyQSV); err != nil || got.name != "hevc_qsv" {
		t.Errorf("automatic HEVC selection = %q, %v; want the encoder that operates (hevc_qsv)", got.name, err)
	}
	none := func(string) bool { return false }
	if got, err := selectFFmpegEncoder(caps, "auto", VideoCodecHEVC, none); err != nil || got.name != "libx265" {
		t.Errorf("automatic HEVC selection without hardware = %q, %v; want libx265", got.name, err)
	}
	if got, err := selectFFmpegEncoder(caps, "amf", VideoCodecHEVC, none); err != nil || got.name != "hevc_amf" {
		t.Errorf("explicit selection = %q, %v; want hevc_amf without a test encode", got.name, err)
	}
	if _, err := selectFFmpegEncoder(caps, "openh264", VideoCodecHEVC, none); err == nil {
		t.Error("openh264 has no HEVC encoder and must fail")
	}
	if _, err := selectFFmpegEncoder(&ffmpegCapabilities{encoders: map[string]bool{}}, "auto", VideoCodecHEVC, none); err == nil {
		t.Error("automatic HEVC selection with no encoder must fail")
	}
}

func TestFFmpegBitrate(t *testing.T) {
	tests := []struct {
		cfg   CaptureConfig
		codec VideoCodec
		want  int
	}{
		{CaptureConfig{Bitrate: 7000, MaxWidth: 3840, MaxHeight: 2160}, VideoCodecHEVC, 7000},
		{CaptureConfig{MaxWidth: 1920, MaxHeight: 1080, FPS: 30}, VideoCodecH264, 11963},
		{CaptureConfig{}, VideoCodecH264, 11963},
		{CaptureConfig{MaxWidth: 3840, MaxHeight: 2160, FPS: 30}, VideoCodecHEVC, 24883},
		{CaptureConfig{MaxWidth: 3840, MaxHeight: 2160, FPS: 60}, VideoCodecHEVC, 40000},
		{CaptureConfig{MaxWidth: 1280, MaxHeight: 720, FPS: 15}, VideoCodecH264, 4000},
	}
	for _, tt := range tests {
		if got := ffmpegBitrateKbps(tt.cfg, tt.codec); got != tt.want {
			t.Errorf("ffmpegBitrateKbps(%+v, %s) = %d, want %d", tt.cfg, tt.codec, got, tt.want)
		}
	}
}

func TestFFmpegHEVCArgs(t *testing.T) {
	cfg := CaptureConfig{FPS: 30, MaxWidth: 3840, MaxHeight: 2160}
	args := strings.Join(ffmpegCaptureArgs(cfg, ffmpegHEVCEncoders["nvenc"], "ddagrab"), " ")
	for _, want := range []string{"-c:v hevc_nvenc", "scale=3840:2160:", "-b:v 24883k", "-f hevc pipe:1"} {
		if !strings.Contains(args, want) {
			t.Errorf("HEVC arguments do not contain %q:\n%s", want, args)
		}
	}
	// A bitstream filter stops FFmpeg with hevc_amf (version 0.2.0 problem).
	for _, method := range []string{"nvenc", "qsv", "amf", "mf"} {
		if hw := strings.Join(ffmpegCaptureArgs(cfg, ffmpegHEVCEncoders[method], "ddagrab"), " "); strings.Contains(hw, "-bsf") {
			t.Errorf("%s HEVC arguments must not have a bitstream filter:\n%s", method, hw)
		}
	}
	args = strings.Join(ffmpegCaptureArgs(cfg, ffmpegHEVCEncoders["none"], "test"), " ")
	if !strings.Contains(args, "aud=1") || strings.Contains(args, "-bsf:v") {
		t.Errorf("x265 writes its delimiters itself:\n%s", args)
	}
}

// hevcNAL builds a NAL unit with a two-byte HEVC header and one payload byte.
func hevcNAL(typ byte, payload byte) []byte {
	return []byte{0, 0, 0, 1, typ << 1, 1, payload}
}

func TestAnnexBAccessUnitReader(t *testing.T) {
	const firstSlice, nextSlice = 0x80, 0x00
	var stream []byte
	// Frame 1: AUD, VPS, SPS, PPS, IDR in two slices.
	for _, nal := range [][]byte{hevcNAL(35, 0), hevcNAL(32, 0), hevcNAL(33, 0), hevcNAL(34, 0), hevcNAL(19, firstSlice), hevcNAL(19, nextSlice)} {
		stream = append(stream, nal...)
	}
	// Frame 2: AUD and one slice. Frame 3: no AUD, found by the slice flag.
	stream = append(stream, hevcNAL(35, 0)...)
	stream = append(stream, hevcNAL(1, firstSlice)...)
	stream = append(stream, hevcNAL(1, firstSlice)...)
	// Frame 4 starts and closes frame 3.
	stream = append(stream, hevcNAL(35, 0)...)
	stream = append(stream, hevcNAL(1, firstSlice)...)

	clock := time.Unix(1000, 0)
	reader := newAnnexBAccessUnitReader(iotest1ByteReader{bytes.NewReader(stream)})
	reader.now = func() time.Time { clock = clock.Add(time.Millisecond); return clock }

	wantNALs := []int{6, 2, 1}
	var last time.Time
	for i, want := range wantNALs {
		unit, err := reader.ReadVideoAccessUnit()
		if err != nil {
			t.Fatalf("access unit %d: %v", i+1, err)
		}
		if got := len(splitAnnexBAccessUnit(unit.AnnexB)); got != want {
			t.Errorf("access unit %d has %d NAL units, want %d", i+1, got, want)
		}
		if !unit.PTS.After(last) {
			t.Errorf("access unit %d has time stamp %v, not after %v", i+1, unit.PTS, last)
		}
		last = unit.PTS
	}
	// The last frame has no following unit, so the stream end is reported.
	if _, err := reader.ReadVideoAccessUnit(); err != io.EOF {
		t.Errorf("read after the last complete unit = %v, want io.EOF", err)
	}
}

// iotest1ByteReader returns one byte for each Read call. It proves that the
// reader does not depend on how the pipe splits the stream.
type iotest1ByteReader struct{ r io.Reader }

func (o iotest1ByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// A hardware encoder writes the parameter sets only before the first key
// frame. The reader must put them before each later key frame.
func TestAnnexBAccessUnitReaderRepeatsParameterSets(t *testing.T) {
	const firstSlice = 0x80
	var stream []byte
	for _, nal := range [][]byte{
		hevcNAL(32, 1), hevcNAL(33, 2), hevcNAL(34, 3), hevcNAL(19, firstSlice), // key frame with sets
		hevcNAL(1, firstSlice),  // normal frame
		hevcNAL(19, firstSlice), // key frame without sets
		hevcNAL(34, 9), hevcNAL(20, firstSlice), // key frame with a new PPS only
		hevcNAL(1, firstSlice), // closes the frame before
	} {
		stream = append(stream, nal...)
	}
	reader := newAnnexBAccessUnitReader(bytes.NewReader(stream))
	types := func(unit VideoAccessUnit) (out []byte, payloads []byte) {
		for _, nal := range splitAnnexBAccessUnit(unit.AnnexB) {
			raw := stripStartCode(nal)
			out = append(out, hevcNALType(raw))
			payloads = append(payloads, raw[2])
		}
		return out, payloads
	}
	want := [][]byte{{32, 33, 34, 19}, {1}, {32, 33, 34, 19}, {32, 33, 34, 20}}
	for i, wantTypes := range want {
		unit, err := reader.ReadVideoAccessUnit()
		if err != nil {
			t.Fatalf("access unit %d: %v", i+1, err)
		}
		got, payloads := types(unit)
		if !bytes.Equal(got, wantTypes) {
			t.Errorf("access unit %d has NAL types %v, want %v", i+1, got, wantTypes)
		}
		if i == 3 && (len(payloads) < 3 || payloads[2] != 9) {
			t.Errorf("access unit 4 must have the new PPS one time, got payloads %v", payloads)
		}
	}
}
