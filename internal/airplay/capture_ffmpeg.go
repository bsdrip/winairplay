package airplay

// FFmpeg capture backend.
//
// This file is part of winairplay and is not in doubletake. doubletake does
// screen capture with GStreamer on Linux. Windows has no usable GStreamer
// installation by default, so winairplay starts FFmpeg, which captures the
// desktop with the Desktop Duplication API (the ddagrab filter), encodes
// H.264 and writes an Annex B byte stream to its standard output. The rest of
// the sender reads that stream through the same ScreenCapture type.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const ffmpegStartupProbeTimeout = 8 * time.Second

var (
	ffmpegPathMu sync.Mutex
	ffmpegPath   string
)

// SetFFmpegPath selects the FFmpeg program. An empty value restores the
// automatic search.
func SetFFmpegPath(path string) {
	ffmpegPathMu.Lock()
	ffmpegPath = path
	ffmpegPathMu.Unlock()
}

// useFFmpegCapture reports whether capture uses FFmpeg and not GStreamer.
// FFmpeg is the only backend on Windows. On other systems the environment
// variable WINAIRPLAY_CAPTURE=ffmpeg selects it for tests.
func useFFmpegCapture() bool {
	return runtime.GOOS == "windows" || os.Getenv("WINAIRPLAY_CAPTURE") == "ffmpeg"
}

// FindFFmpeg returns the FFmpeg program to use. The search order is: the path
// from SetFFmpegPath, the environment variable WINAIRPLAY_FFMPEG, the
// directory of this program (also its ffmpeg and ffmpeg\bin subdirectories),
// and then PATH.
func FindFFmpeg() (string, error) {
	ffmpegPathMu.Lock()
	explicit := ffmpegPath
	ffmpegPathMu.Unlock()
	if explicit == "" {
		explicit = os.Getenv("WINAIRPLAY_FFMPEG")
	}
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("FFmpeg not found at %q: %w", explicit, err)
		}
		return explicit, nil
	}
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name = "ffmpeg.exe"
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, candidate := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "ffmpeg", name),
			filepath.Join(dir, "ffmpeg", "bin", name),
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("FFmpeg not found: put %s next to winairplay or add it to PATH", name)
}

// ffmpegCapabilities is the part of an FFmpeg build that capture depends on.
type ffmpegCapabilities struct {
	path     string
	ddagrab  bool
	gdigrab  bool
	encoders map[string]bool
}

var (
	ffmpegCapsMu    sync.Mutex
	ffmpegCapsCache = map[string]*ffmpegCapabilities{}
)

func ffmpegQuery(ctx context.Context, path string, args ...string) (string, error) {
	queryCtx, cancel := context.WithTimeout(normalizeContext(ctx), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(queryCtx, path, append([]string{"-hide_banner", "-nostdin"}, args...)...)
	setChildProcAttr(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", filepath.Base(path), strings.Join(args, " "), err)
	}
	return string(out), nil
}

// hasListedName reports whether an FFmpeg list (-filters, -encoders, -devices)
// has a row for name. The name is the second column of a row.
func hasListedName(listing, name string) bool {
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == name {
			return true
		}
	}
	return false
}

func probeFFmpeg(ctx context.Context) (*ffmpegCapabilities, error) {
	path, err := FindFFmpeg()
	if err != nil {
		return nil, err
	}
	ffmpegCapsMu.Lock()
	cached := ffmpegCapsCache[path]
	ffmpegCapsMu.Unlock()
	if cached != nil {
		return cached, nil
	}
	encoders, err := ffmpegQuery(ctx, path, "-encoders")
	if err != nil {
		return nil, err
	}
	filters, err := ffmpegQuery(ctx, path, "-filters")
	if err != nil {
		return nil, err
	}
	devices, _ := ffmpegQuery(ctx, path, "-devices")
	caps := &ffmpegCapabilities{
		path:     path,
		ddagrab:  hasListedName(filters, "ddagrab"),
		gdigrab:  hasListedName(devices, "gdigrab"),
		encoders: map[string]bool{},
	}
	for _, name := range []string{"libx264", "libopenh264", "h264_mf", "h264_nvenc", "h264_qsv", "h264_amf"} {
		caps.encoders[name] = hasListedName(encoders, name)
	}
	ffmpegCapsMu.Lock()
	ffmpegCapsCache[path] = caps
	ffmpegCapsMu.Unlock()
	return caps, nil
}

// ffmpegEncoder is one H.264 encoder selection.
type ffmpegEncoder struct {
	name   string // FFmpeg encoder name
	label  string
	pixFmt string
}

// selectFFmpegEncoder maps the encoder preference to an FFmpeg encoder.
// "auto" selects a software encoder, because the list of encoders in an FFmpeg
// build does not show which graphics hardware is installed.
func selectFFmpegEncoder(caps *ffmpegCapabilities, hwaccel string) (ffmpegEncoder, error) {
	if hwaccel == "" {
		hwaccel = "auto"
	}
	explicit := map[string]ffmpegEncoder{
		"nvenc":    {name: "h264_nvenc", label: "NVIDIA NVENC", pixFmt: "nv12"},
		"qsv":      {name: "h264_qsv", label: "Intel Quick Sync", pixFmt: "nv12"},
		"amf":      {name: "h264_amf", label: "AMD AMF", pixFmt: "nv12"},
		"mf":       {name: "h264_mf", label: "Media Foundation", pixFmt: "nv12"},
		"openh264": {name: "libopenh264", label: "OpenH264 software", pixFmt: "yuv420p"},
		"none":     {name: "libx264", label: "x264 software", pixFmt: "yuv420p"},
	}
	if hwaccel == "auto" {
		for _, method := range []string{"none", "openh264", "mf"} {
			if encoder := explicit[method]; caps.encoders[encoder.name] {
				return encoder, nil
			}
		}
		return ffmpegEncoder{}, fmt.Errorf("this FFmpeg build has no H.264 encoder (libx264, libopenh264 or h264_mf)")
	}
	encoder, ok := explicit[hwaccel]
	if !ok {
		return ffmpegEncoder{}, fmt.Errorf("encoder %q is not available with FFmpeg capture (use auto, none, nvenc, qsv, amf, mf, or openh264)", hwaccel)
	}
	if !caps.encoders[encoder.name] {
		return ffmpegEncoder{}, fmt.Errorf("this FFmpeg build does not include the %s encoder (%s)", encoder.label, encoder.name)
	}
	return encoder, nil
}

func prepareFFmpegCapture(ctx context.Context, cfg CaptureConfig, kind capturePreparationKind) (*CapturePreparation, error) {
	if cfg.VideoCodec == VideoCodecHEVC {
		return nil, fmt.Errorf("FFmpeg capture supports H.264 only")
	}
	caps, err := probeFFmpeg(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := selectFFmpegEncoder(caps, cfg.HWAccel); err != nil {
		return nil, err
	}
	if kind == capturePreparationFFmpeg {
		if runtime.GOOS != "windows" {
			return nil, fmt.Errorf("FFmpeg screen capture is available only on Windows; use test mode on this system")
		}
		if !caps.ddagrab && !caps.gdigrab {
			return nil, fmt.Errorf("this FFmpeg build has no Windows screen capture (ddagrab or gdigrab)")
		}
	}
	return &CapturePreparation{ctx: ctx, cfg: cfg, kind: kind}, nil
}

// ffmpegScaleFilter fits the captured image into the receiver canvas and adds
// black borders, as the GStreamer path does. H.264 4:2:0 needs even dimensions.
func ffmpegScaleFilter(maxWidth, maxHeight int, pixFmt string) string {
	const color = "out_color_matrix=bt709:out_range=tv"
	maxWidth &^= 1
	maxHeight &^= 1
	if maxWidth <= 0 || maxHeight <= 0 {
		return fmt.Sprintf("scale=trunc(iw/2)*2:trunc(ih/2)*2:%s,setsar=1,format=%s", color, pixFmt)
	}
	return fmt.Sprintf(
		"scale=%d:%d:force_original_aspect_ratio=decrease:force_divisible_by=2:%s,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=%s",
		maxWidth, maxHeight, color, maxWidth, maxHeight, pixFmt)
}

func ffmpegEncoderArgs(encoder ffmpegEncoder, bitrate, fps int) []string {
	keyint := keyframeIntervalFrames(fps)
	maxrate := bitrate + bitrate/4
	vbv := vbvBufferKbit(bitrate, fps)
	rate := []string{"-b:v", fmt.Sprintf("%dk", bitrate), "-g", fmt.Sprint(keyint), "-bf", "0"}
	var args []string
	switch encoder.name {
	case "libx264":
		args = append([]string{"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency"}, rate...)
		args = append(args,
			"-maxrate", fmt.Sprintf("%dk", maxrate),
			"-bufsize", fmt.Sprintf("%dk", vbv),
			"-x264-params", "aud=1:repeat-headers=1:sliced-threads=1",
		)
	case "libopenh264":
		args = append([]string{"-c:v", "libopenh264", "-allow_skip_frames", "0"}, rate...)
	case "h264_nvenc":
		args = append([]string{"-c:v", "h264_nvenc", "-preset", "p3", "-tune", "ll", "-rc", "cbr", "-zerolatency", "1", "-aud", "1", "-forced-idr", "1"}, rate...)
		args = append(args, "-maxrate", fmt.Sprintf("%dk", maxrate), "-bufsize", fmt.Sprintf("%dk", vbv))
	case "h264_qsv":
		args = append([]string{"-c:v", "h264_qsv", "-preset", "veryfast", "-async_depth", "1", "-look_ahead", "0", "-aud", "1"}, rate...)
		args = append(args, "-maxrate", fmt.Sprintf("%dk", maxrate), "-bufsize", fmt.Sprintf("%dk", vbv))
	case "h264_amf":
		args = append([]string{"-c:v", "h264_amf", "-usage", "ultralowlatency", "-rc", "cbr", "-aud", "1"}, rate...)
	case "h264_mf":
		args = append([]string{"-c:v", "h264_mf", "-rate_control", "cbr", "-scenario", "display_remoting"}, rate...)
	}
	if encoder.name != "libx264" {
		// The receiver needs SPS and PPS before each IDR frame. x264 repeats
		// them itself; the bitstream filter does it for the other encoders.
		args = append(args, "-bsf:v", "dump_extra=freq=keyframe")
	}
	return args
}

// ffmpegCaptureArgs builds the FFmpeg command line. source is "ddagrab",
// "gdigrab" or "test".
func ffmpegCaptureArgs(cfg CaptureConfig, encoder ffmpegEncoder, source string) []string {
	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	bitrate := captureBitrateKbps(cfg)
	cursor := 0
	if cfg.ShowCursor {
		cursor = 1
	}
	display := cfg.Display
	if display < 0 {
		display = 0
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning"}
	switch source {
	case "ddagrab":
		// ddagrab gives Direct3D 11 frames. hwdownload copies them to system
		// memory so that the scale filter and each encoder can use them.
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf(
			"ddagrab=output_idx=%d:framerate=%d:draw_mouse=%d,hwdownload,format=bgra", display, fps, cursor))
	case "gdigrab":
		args = append(args, "-f", "gdigrab", "-framerate", fmt.Sprint(fps), "-draw_mouse", fmt.Sprint(cursor), "-i", "desktop")
	default:
		args = append(args, "-re", "-f", "lavfi", "-i", fmt.Sprintf(
			"testsrc2=size=%dx%d:rate=%d", testCaptureWidth, testCaptureHeight, fps))
	}
	args = append(args, "-an", "-vf", ffmpegScaleFilter(cfg.MaxWidth, cfg.MaxHeight, encoder.pixFmt))
	args = append(args, ffmpegEncoderArgs(encoder, bitrate, fps)...)
	args = append(args,
		"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709", "-color_range", "tv",
		"-flags", "+low_delay", "-flush_packets", "1", "-f", "h264", "pipe:1")
	return args
}

func startFFmpegCapture(startupCtx, lifetimeCtx context.Context, cfg CaptureConfig, test bool) (*ScreenCapture, error) {
	if normalizeVideoCodec(cfg.VideoCodec) != VideoCodecH264 {
		return nil, fmt.Errorf("FFmpeg capture supports H.264 only")
	}
	startupCtx = normalizeContext(startupCtx)
	caps, err := probeFFmpeg(startupCtx)
	if err != nil {
		return nil, err
	}
	encoder, err := selectFFmpegEncoder(caps, cfg.HWAccel)
	if err != nil {
		return nil, err
	}
	log.Printf("[CAPTURE] using %s encoding (%s)", encoder.label, encoder.name)

	var sources []string
	switch {
	case test:
		sources = []string{"test"}
	default:
		if caps.ddagrab {
			sources = append(sources, "ddagrab")
		}
		// gdigrab captures the full virtual desktop. Use it only as the
		// fallback for the first display.
		if caps.gdigrab && (cfg.Display <= 0 || !caps.ddagrab) {
			sources = append(sources, "gdigrab")
		}
	}
	var lastErr error
	for _, source := range sources {
		capture, err := startFFmpegCaptureSource(startupCtx, lifetimeCtx, caps.path, cfg, encoder, source)
		if err == nil {
			return capture, nil
		}
		lastErr = err
		log.Printf("[CAPTURE] %s capture failed: %v", source, err)
		if startupCtx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no screen capture source is available")
	}
	return nil, lastErr
}

func startFFmpegCaptureSource(startupCtx, lifetimeCtx context.Context, path string, cfg CaptureConfig, encoder ffmpegEncoder, source string) (*ScreenCapture, error) {
	startupCtx, cancelStartup := context.WithTimeout(startupCtx, captureStartupBudget)
	defer cancelStartup()
	captureCtx, cancel := context.WithCancel(normalizeContext(lifetimeCtx))

	fps := cfg.FPS
	if fps <= 0 {
		fps = 30
	}
	args := ffmpegCaptureArgs(cfg, encoder, source)
	dbg("[CAPTURE] %s %s", path, strings.Join(args, " "))
	cmd := exec.CommandContext(captureCtx, path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		_ = stdout.Close()
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}
	waitResult, err := startGStreamerCommand(cmd)
	if err != nil {
		cancel()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	stderrTail := newCaptureStderrTail()
	go logStderr("FFMPEG", stderr, stderrTail)

	capture := &ScreenCapture{
		cmd:    cmd,
		stdout: stdout,
		cancel: cancel,
		waitCh: make(chan struct{}),
		stderr: stderrTail,
	}
	go func() {
		capture.waitErr = <-waitResult
		close(capture.waitCh)
	}()

	// A first start of FFmpeg from disk and the initialization of a hardware
	// encoder can take some seconds on Windows. Use the full startup budget.
	probeTimeout := captureStartupProbeTimeout(fps)
	if probeTimeout < ffmpegStartupProbeTimeout {
		probeTimeout = ffmpegStartupProbeTimeout
	}
	probeCtx, cancelProbe := context.WithTimeout(startupCtx, probeTimeout)
	err = probeVideoCaptureStartup(probeCtx, capture, VideoCodecH264, false)
	cancelProbe()
	if err != nil {
		capture.Stop()
		result := fmt.Errorf("FFmpeg %s capture failed startup validation: %w", source, err)
		if text := capture.stderrText(); text != "" {
			result = fmt.Errorf("%w; FFmpeg output: %s", result, text)
		}
		return nil, result
	}
	return capture, nil
}
