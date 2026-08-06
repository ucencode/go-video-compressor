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

	// Bitrates are bits/sec and 0 when they could not be determined; callers
	// must treat 0 as "unknown" rather than "silent" or "empty".
	VideoBitrate int
	AudioCodec   string // "" when the file carries no audio stream
	AudioBitrate int
}

// Probe reads stream metadata. Duration comes from the container format, which
// is more reliable than the video stream's own duration for many inputs.
func Probe(path string) (VideoInfo, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "stream=codec_type,codec_name,width,height,bit_rate:format=duration,bit_rate",
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
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			BitRate   string `json:"bit_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return VideoInfo{}, fmt.Errorf("parse ffprobe output: %w", err)
	}

	var info VideoInfo
	// A missing or unparseable duration is not fatal: encoding still works,
	// progress just can't be expressed as a percentage.
	info.Duration, _ = strconv.ParseFloat(parsed.Format.Duration, 64)

	var haveVideo bool
	for _, s := range parsed.Streams {
		switch {
		case s.CodecType == "video" && !haveVideo:
			haveVideo = true
			info.Width, info.Height = s.Width, s.Height
			info.VideoBitrate = atoiSafe(s.BitRate)
		case s.CodecType == "audio" && info.AudioCodec == "":
			info.AudioCodec = s.CodecName
			info.AudioBitrate = atoiSafe(s.BitRate)
		}
	}
	if !haveVideo {
		return VideoInfo{}, fmt.Errorf("no video stream found")
	}

	// Only MP4-family containers report per-stream bit_rate; Matroska and WebM
	// omit it entirely, so fall back to measuring the streams themselves.
	if info.AudioCodec != "" && info.AudioBitrate == 0 {
		info.AudioBitrate = measureBitrate(path, "a:0")
	}
	if info.VideoBitrate == 0 {
		// The container total minus audio is cheap and close enough for a
		// ceiling; measuring packets is the fallback when even that is missing.
		if total := atoiSafe(parsed.Format.BitRate); total > info.AudioBitrate {
			info.VideoBitrate = total - info.AudioBitrate
		} else {
			info.VideoBitrate = measureBitrate(path, "v:0")
		}
	}
	return info, nil
}

// measureBitrate estimates a stream's bitrate by summing packet sizes over the
// first probeWindow seconds. Reading the whole stream would mean walking an
// entire multi-gigabyte upload just to pick an encoder ceiling. Returns 0 if the
// stream can't be measured.
func measureBitrate(path, stream string) int {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", stream,
		"-read_intervals", fmt.Sprintf("%%+%d", probeWindow),
		"-show_entries", "packet=size,duration_time",
		"-of", "json",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}

	// JSON rather than csv: ffprobe emits these fields in its own order, not the
	// order they were asked for, and pads some containers with a trailing empty
	// field, so parsing by position reads sizes as durations.
	var parsed struct {
		Packets []struct {
			Size         string `json:"size"`
			DurationTime string `json:"duration_time"`
		} `json:"packets"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		return 0
	}

	var bytes, seconds float64
	for _, p := range parsed.Packets {
		b, err := strconv.ParseFloat(p.Size, 64)
		if err != nil {
			continue
		}
		d, err := strconv.ParseFloat(p.DurationTime, 64)
		if err != nil {
			continue
		}
		bytes += b
		seconds += d
	}
	if seconds <= 0 {
		return 0
	}
	return int(bytes * 8 / seconds)
}

const probeWindow = 30 // seconds of packets sampled by measureBitrate

func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// EncodeRequest describes one transcode.
type EncodeRequest struct {
	Input  string
	Output string
	Preset Preset
	Source VideoInfo // from Probe; caps the preset so a weak source is never "upgraded"
}

// Encode transcodes Input to Output, calling onProgress with a 0..1 fraction as
// ffmpeg advances. onProgress may be nil.
func Encode(ctx context.Context, enc Encoder, r EncodeRequest, onProgress func(float64)) error {
	args := []string{
		"-hide_banner", "-nostdin", "-y",
		"-i", r.Input,
		"-map", "0:v:0", "-map", "0:a?",
		"-vf", scaleFilter(r.Preset.MaxShortSide),
		"-pix_fmt", "yuv420p",
	}
	args = append(args, videoArgs(enc, r.Preset, r.Source)...)
	args = append(args, audioArgs(r.Preset, r.Source)...)
	args = append(args,
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
	readProgress(stdout, r.Source.Duration, onProgress)

	if err := cmd.Wait(); err != nil {
		if msg := stderr.String(); msg != "" {
			return fmt.Errorf("ffmpeg: %s", msg)
		}
		return fmt.Errorf("ffmpeg: %w", err)
	}
	return nil
}

// scaleFilter builds a -vf value that caps the shorter dimension at max,
// leaving the longer one to follow the aspect ratio. Constraining the height
// instead would penalise portrait video: a 1080x1920 phone clip would come out
// at 608x1080 under the same preset that leaves 1920x1080 untouched.
//
// Each branch downscales only — min() leaves already-small videos alone — and
// rounds to an even number, which yuv420p requires; -2 does the same rounding
// for the unconstrained side.
func scaleFilter(max int) string {
	landscape := fmt.Sprintf("trunc(min(%d,ih)/2)*2", max)
	portrait := fmt.Sprintf("trunc(min(%d,iw)/2)*2", max)
	return fmt.Sprintf("scale=w='if(gt(iw,ih),-2,%s)':h='if(gt(iw,ih),%s,-2)'", portrait, landscape)
}

func videoArgs(enc Encoder, p Preset, src VideoInfo) []string {
	var args []string
	if enc.Name == "h264_nvenc" {
		// -b:v 0 puts NVENC's VBR mode into pure constant-quality.
		args = []string{
			"-c:v", "h264_nvenc",
			"-preset", "p5",
			"-tune", "hq",
			"-rc", "vbr",
			"-cq", strconv.Itoa(p.Quality),
			"-b:v", "0",
			"-spatial-aq", "1",
		}
	} else {
		args = []string{
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", strconv.Itoa(p.Quality),
		}
	}

	// Constant quality alone will happily spend more bits than the source ever
	// had: re-encoding an already-compressed 86kbps clip at CRF 21 produces a
	// ~220kbps file that is 2.5x larger and no better, because the CRF target is
	// faithfully reproducing the source's own compression artifacts. Ceiling the
	// rate at what the source used makes that impossible. For normal footage the
	// ceiling sits far above what CRF asks for and never binds — and when we
	// downscale, the source's rate is more headroom still.
	if src.VideoBitrate > 0 {
		args = append(args,
			"-maxrate", strconv.Itoa(src.VideoBitrate),
			"-bufsize", strconv.Itoa(src.VideoBitrate*2),
		)
	}
	return args
}

// audioBitrateLadder holds the bitrates encoders and encoding GUIs actually use.
// Measured rates drift a little from their nominal value — a 128k AAC stream
// probes as ~123k — so a source is snapped up to its rung before being compared
// against the preset, otherwise that noise alone would talk us into 123k.
var audioBitrateLadder = []int{32, 48, 64, 96, 128, 160, 192, 224, 256, 320}

// audioArgs picks the audio encoding for a source, capping the preset at what
// the source actually carries. Audio bitrate is a ceiling, not a target: nothing
// is recovered by encoding a 128k source at 160k, it just costs bytes.
func audioArgs(p Preset, src VideoInfo) []string {
	if src.AudioCodec == "" {
		return nil // -map 0:a? already made the audio stream optional
	}

	target := p.AudioKbps
	// Lossless sources carry no bitrate worth preserving — any lossy target is a
	// downgrade — so they take the preset as-is.
	if src.AudioBitrate > 0 && !isLosslessAudio(src.AudioCodec) {
		equivalent := float64(src.AudioBitrate) / 1000 * aacEquivalence(src.AudioCodec)
		source := snapUpToLadder(int(equivalent + 0.5))
		if source < target {
			target = source
		}
		// An AAC source already at or under the preset is best left alone:
		// re-encoding it is a second lossy pass over already-lossy audio, and
		// buys no space at the same bitrate.
		if src.AudioCodec == "aac" && source <= p.AudioKbps {
			return []string{"-c:a", "copy"}
		}
	}
	return []string{"-c:a", "aac", "-b:a", fmt.Sprintf("%dk", target)}
}

// aacEquivalence converts a source bitrate into the AAC bitrate that sounds
// about the same. Codecs are not interchangeable at equal rates: 96k Opus is
// roughly 128k AAC, so capping AAC at 96k just because the source said 96k
// would throw away quality the source actually had.
func aacEquivalence(codec string) float64 {
	switch codec {
	case "opus", "vorbis":
		return 1.3
	default:
		// AAC maps to itself; MP3/AC-3 and friends are less efficient than AAC,
		// so treating them 1:1 errs toward keeping bits rather than dropping them.
		return 1
	}
}

func isLosslessAudio(codec string) bool {
	switch codec {
	case "flac", "alac", "truehd", "mlp", "wavpack", "tta", "ape":
		return true
	}
	return strings.HasPrefix(codec, "pcm_")
}

func snapUpToLadder(kbps int) int {
	for _, rung := range audioBitrateLadder {
		if kbps <= rung {
			return rung
		}
	}
	return kbps
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
