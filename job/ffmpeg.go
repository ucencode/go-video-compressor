package job

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Encoder is the video encoder the server settled on at startup.
type Encoder struct {
	Name    string // ffmpeg encoder name, e.g. "h264_nvenc"
	Display string // human-readable, surfaced in the UI
	HW      bool
}

var (
	detectOnce sync.Once
	detected   Encoder
)

// hwCandidates are tried in order; the first one that survives a throwaway
// encode wins. Listing an encoder in `ffmpeg -encoders` is not enough — NVENC
// is compiled in on machines with no NVIDIA driver at all.
var hwCandidates = []Encoder{
	{Name: "h264_nvenc", Display: "h264_nvenc (GPU)", HW: true},
}

// DetectEncoder picks the best available encoder. The result is cached, so the
// probe encode runs at most once per process.
func DetectEncoder() Encoder {
	detectOnce.Do(func() {
		for _, c := range hwCandidates {
			if encoderWorks(c.Name) {
				detected = c
				return
			}
		}
		detected = Encoder{Name: "libx264", Display: "libx264 (CPU)"}
	})
	return detected
}

// encoderWorks runs a tiny synthetic encode to confirm the encoder is not just
// present but actually usable on this machine.
func encoderWorks(name string) bool {
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "nullsrc=s=256x256:d=0.1",
		"-c:v", name,
		"-f", "null", "-",
	)
	return cmd.Run() == nil
}

// VideoInfo is the subset of ffprobe output the pipeline needs.
type VideoInfo struct {
	Duration float64
	Width    int
	Height   int
}

// Probe reads stream metadata. Duration comes from the container format, which
// is more reliable than the video stream's own duration for many inputs.
func Probe(path string) (VideoInfo, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
		"-of", "json",
		path,
	)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := stderr.String(); msg != "" {
			return VideoInfo{}, fmt.Errorf("not a readable video file: %s", msg)
		}
		return VideoInfo{}, fmt.Errorf("ffprobe failed: %w", err)
	}

	var parsed struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return VideoInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}
	if len(parsed.Streams) == 0 {
		return VideoInfo{}, fmt.Errorf("no video stream found")
	}

	info := VideoInfo{
		Width:  parsed.Streams[0].Width,
		Height: parsed.Streams[0].Height,
	}
	// A missing or unparseable duration is not fatal: encoding still works,
	// progress just can't be expressed as a percentage.
	info.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)
	return info, nil
}

// EncodeRequest describes one transcode.
type EncodeRequest struct {
	Input    string
	Output   string
	Preset   Preset
	Duration float64 // seconds, from Probe; 0 disables progress reporting
}

// Encode transcodes Input to Output, calling onProgress with a 0..1 fraction as
// ffmpeg advances. onProgress may be nil.
func Encode(ctx context.Context, enc Encoder, r EncodeRequest, onProgress func(float64)) error {
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", r.Input,
		"-map", "0:v:0", "-map", "0:a?",
		// Downscale only: min() leaves already-small videos untouched, and -2
		// keeps the width even (required by yuv420p).
		"-vf", fmt.Sprintf("scale=-2:'min(%d,ih)'", r.Preset.MaxHeight),
		"-pix_fmt", "yuv420p",
	}
	args = append(args, videoArgs(enc, r.Preset)...)
	args = append(args,
		"-c:a", "aac", "-b:a", r.Preset.AudioBitrate,
		"-movflags", "+faststart",
		"-progress", "pipe:1", "-nostats",
		r.Output,
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	// Drain the progress stream to completion before Wait, which closes the pipe.
	readProgress(stdout, r.Duration, onProgress)

	if err := cmd.Wait(); err != nil {
		if msg := stderr.String(); msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

func videoArgs(enc Encoder, p Preset) []string {
	if enc.Name == "h264_nvenc" {
		// -b:v 0 puts NVENC's VBR mode into pure constant-quality.
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p5",
			"-tune", "hq",
			"-rc", "vbr",
			"-cq", strconv.Itoa(p.Quality),
			"-b:v", "0",
			"-spatial-aq", "1",
		}
	}
	return []string{
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", strconv.Itoa(p.Quality),
	}
}

// readProgress consumes ffmpeg's -progress stream (key=value lines) and
// translates elapsed output time into a completion fraction.
func readProgress(stdout io.Reader, duration float64, onProgress func(float64)) {
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok || onProgress == nil {
			continue
		}

		switch key {
		case "out_time_us", "out_time_ms":
			// Both keys are reported in microseconds; out_time_ms is a
			// long-standing ffmpeg misnomer.
			us, err := strconv.ParseFloat(value, 64)
			if err != nil || duration <= 0 {
				continue
			}
			frac := (us / 1e6) / duration
			onProgress(clamp01(frac))
		case "progress":
			if value == "end" {
				onProgress(1)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("reading ffmpeg progress: %v", err)
	}
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// tailBuffer keeps only the last maxTail bytes written to it. ffmpeg can emit a
// lot of stderr; the tail is where the actual failure reason lives.
type tailBuffer struct {
	buf []byte
}

const maxTail = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > maxTail {
		t.buf = t.buf[len(t.buf)-maxTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	// The last few lines carry the error; everything above is progress noise.
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.TrimSpace(strings.Join(lines, "; "))
}
