package job

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestAudioArgs(t *testing.T) {
	quality, _ := LookupPreset("quality")   // 160k
	balanced, _ := LookupPreset("balanced") // 128k
	small, _ := LookupPreset("small")       // 96k

	tests := []struct {
		name string
		p    Preset
		src  VideoInfo
		want []string
	}{{
		name: "aac source below preset is copied rather than re-encoded",
		p:    quality,
		src:  VideoInfo{AudioCodec: "aac", AudioBitrate: 122802},
		want: []string{"-c:a", "copy"},
	}, {
		name: "aac source above preset is capped at the preset",
		p:    small,
		src:  VideoInfo{AudioCodec: "aac", AudioBitrate: 192000},
		want: []string{"-c:a", "aac", "-b:a", "96k"},
	}, {
		name: "mp3 source below preset is re-encoded at the source rate",
		p:    quality,
		src:  VideoInfo{AudioCodec: "mp3", AudioBitrate: 64393},
		want: []string{"-c:a", "aac", "-b:a", "64k"},
	}, {
		name: "opus is given the aac bitrate that matches its quality",
		p:    quality,
		src:  VideoInfo{AudioCodec: "opus", AudioBitrate: 96800},
		want: []string{"-c:a", "aac", "-b:a", "128k"},
	}, {
		name: "lossless source takes the preset untouched",
		p:    balanced,
		src:  VideoInfo{AudioCodec: "flac", AudioBitrate: 900000},
		want: []string{"-c:a", "aac", "-b:a", "128k"},
	}, {
		name: "unknown bitrate falls back to the preset",
		p:    balanced,
		src:  VideoInfo{AudioCodec: "aac"},
		want: []string{"-c:a", "aac", "-b:a", "128k"},
	}, {
		name: "no audio stream produces no audio args",
		p:    balanced,
		src:  VideoInfo{},
		want: nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := audioArgs(tt.p, tt.src)
			if strings.Join(got, " ") != strings.Join(tt.want, " ") {
				t.Errorf("audioArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestVideoArgsRateCeiling(t *testing.T) {
	p, _ := LookupPreset("quality")
	cpu := Encoder{Name: "libx264"}

	got := strings.Join(videoArgs(cpu, p, VideoInfo{VideoBitrate: 86000}), " ")
	if !strings.Contains(got, "-maxrate 86000") || !strings.Contains(got, "-bufsize 172000") {
		t.Errorf("expected the source bitrate as a ceiling, got %q", got)
	}

	got = strings.Join(videoArgs(cpu, p, VideoInfo{}), " ")
	if strings.Contains(got, "-maxrate") {
		t.Errorf("an unknown source bitrate must not impose a ceiling, got %q", got)
	}
}

// TestEncodeDoesNotInflateWeakSource is the regression this whole path exists
// for: constant quality alone re-encodes an already-compressed clip at several
// times its original bitrate, so the "compressor" hands back a bigger file that
// looks no better.
func TestEncodeDoesNotInflateWeakSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	dir := t.TempDir()
	src := dir + "/src.mp4"
	build := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=1280x720:rate=30:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-crf", "40", "-c:a", "aac", "-b:a", "128k", "-y", src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	info, err := Probe(src)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	p, _ := LookupPreset("quality") // the preset that overshoots hardest
	out := dir + "/out.mp4"
	req := EncodeRequest{Input: src, Output: out, Preset: p, Source: info}
	if err := Encode(context.Background(), Encoder{Name: "libx264"}, req, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Probe(out)
	if err != nil {
		t.Fatalf("probing output: %v", err)
	}
	// The ceiling is a VBV constraint, so brief overshoot is expected; what must
	// not happen is the multiple-times-larger result of an unconstrained CRF.
	if limit := info.VideoBitrate * 5 / 4; got.VideoBitrate > limit {
		t.Errorf("output video bitrate %d exceeds source %d beyond tolerance (%d)",
			got.VideoBitrate, info.VideoBitrate, limit)
	}
	if got.AudioCodec != "aac" {
		t.Errorf("AudioCodec = %q, want aac", got.AudioCodec)
	}
}

// TestProbeBitrates covers the containers that report no per-stream bit_rate,
// where the fallback measurement is the only thing standing between us and
// treating every Matroska upload as "bitrate unknown".
func TestProbeBitrates(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	tests := []struct {
		name     string
		file     string
		args     []string
		codec    string
		wantKbps int // approximate; measurement drifts from the nominal rate
	}{
		{"mp4 reports stream bitrate", "aac.mp4", []string{"-c:a", "aac", "-b:a", "128k"}, "aac", 128},
		{"mkv needs the packet fallback", "aac.mkv", []string{"-c:a", "aac", "-b:a", "96k"}, "aac", 96},
		{"webm needs the packet fallback", "opus.webm", []string{"-c:v", "libvpx-vp9", "-b:v", "200k", "-c:a", "libopus", "-b:a", "96k"}, "opus", 96},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := t.TempDir() + "/" + tt.file
			args := []string{
				"-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
			}
			if !strings.Contains(strings.Join(tt.args, " "), "-c:v") {
				args = append(args, "-c:v", "libx264", "-crf", "30")
			}
			args = append(args, tt.args...)
			if out, err := exec.Command("ffmpeg", append(args, "-y", path)...).CombinedOutput(); err != nil {
				t.Fatalf("building fixture: %v: %s", err, out)
			}

			info, err := Probe(path)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if info.AudioCodec != tt.codec {
				t.Errorf("AudioCodec = %q, want %q", info.AudioCodec, tt.codec)
			}
			if got := info.AudioBitrate / 1000; got < tt.wantKbps*8/10 || got > tt.wantKbps*13/10 {
				t.Errorf("AudioBitrate = %dkbps, want roughly %dkbps", got, tt.wantKbps)
			}
			if info.VideoBitrate <= 0 {
				t.Errorf("VideoBitrate = %d, want a positive estimate", info.VideoBitrate)
			}
		})
	}
}
