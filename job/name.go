package job

import (
	"crypto/rand"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewID returns a fresh identifier for a job or a file. ULIDs sort by creation
// time, which is why job and file listings can order by id or created_at
// interchangeably.
func NewID() string {
	ms := ulid.Timestamp(time.Now())
	id, err := ulid.New(ms, rand.Reader)
	if err != nil {
		panic(err)
	}
	return id.String()
}

// OutputName turns "holiday.mov" into "holiday-compressed.mp4". It is the name
// stored on the compressed file's row and offered to the browser on download.
func OutputName(orig string) string {
	base := strings.TrimSuffix(filepath.Base(orig), filepath.Ext(orig))
	if base == "" {
		base = "video"
	}
	return base + "-compressed.mp4"
}
