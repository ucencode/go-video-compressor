package job

// Preset is a user-selectable compression profile. Quality is expressed on the
// x264 CRF scale and reused as the NVENC CQ value; the two scales are close
// enough that a single number keeps the presets consistent across encoders.
type Preset struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`

	// MaxShortSide caps the smaller of the two dimensions, so "1080p" means the
	// same amount of detail whether the video is landscape or portrait. Only ever
	// downscales, never upscales.
	MaxShortSide int `json:"-"`
	Quality      int `json:"-"` // CRF (libx264) / CQ (nvenc)
	AudioKbps    int `json:"-"` // ceiling, not a target: see audioArgs
}

// Presets is ordered for display; the API returns it as a list so the frontend
// keeps this order instead of an arbitrary map iteration.
var Presets = []Preset{
	{
		Key:          "quality",
		Label:        "High quality",
		Description:  "1080p · near-original",
		MaxShortSide: 1080,
		Quality:      21,
		AudioKbps:    160,
	},
	{
		Key:          "balanced",
		Label:        "Balanced",
		Description:  "1080p · good size",
		MaxShortSide: 1080,
		Quality:      26,
		AudioKbps:    128,
	},
	{
		Key:          "small",
		Label:        "Small",
		Description:  "720p · smallest file",
		MaxShortSide: 720,
		Quality:      30,
		AudioKbps:    96,
	},
}

func LookupPreset(key string) (Preset, bool) {
	for _, p := range Presets {
		if p.Key == key {
			return p, true
		}
	}
	return Preset{}, false
}
