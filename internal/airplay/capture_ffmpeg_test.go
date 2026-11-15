package airplay

import (
	"strings"
	"testing"
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
		got, err := selectFFmpegEncoder(caps, tt.hwaccel)
		if (err != nil) != tt.wantErr || got.name != tt.want {
			t.Errorf("selectFFmpegEncoder(%q) = %q, %v; want %q, error %t", tt.hwaccel, got.name, err, tt.want, tt.wantErr)
		}
	}

	// Without x264, automatic selection uses the next software encoder.
	caps = &ffmpegCapabilities{encoders: map[string]bool{"h264_nvenc": true, "h264_mf": true}}
	if got, err := selectFFmpegEncoder(caps, "auto"); err != nil || got.name != "h264_mf" {
		t.Errorf("automatic selection without x264 = %q, %v; want h264_mf", got.name, err)
	}
	if _, err := selectFFmpegEncoder(&ffmpegCapabilities{encoders: map[string]bool{}}, "auto"); err == nil {
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
	x264 := ffmpegEncoder{name: "libx264", pixFmt: "yuv420p"}

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
	args = strings.Join(ffmpegCaptureArgs(cfg, ffmpegEncoder{name: "h264_nvenc", pixFmt: "nv12"}, "gdigrab"), " ")
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
