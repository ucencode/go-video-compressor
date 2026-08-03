package main

import (
	"database/sql"
	"errors"
	"fmt"
	"go-video-compressor/database"
	"go-video-compressor/job"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

const (
	uploadDir = "./uploads"
	outputDir = "./outputs"
	staticDir = "./static"

	// Bound to all interfaces on purpose: this is a LAN tool. Override with
	// LISTEN_ADDR (e.g. "127.0.0.1:8080") to keep it local.
	defaultListenAddr = "0.0.0.0:8080"

	queueBuffer = 10
	workers     = 1 // one GPU; parallel encodes just slow each other down
	maxUpload   = 8 << 30
)

// File is a row of the file registry. The on-disk path is deliberately not
// serialized — it is server-internal.
type File struct {
	ID        string    `json:"id"`
	FileName  string    `json:"filename"`
	Size      uint64    `json:"size"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

func getPresets(c *gin.Context) {
	c.JSON(http.StatusOK, job.Presets)
}

func getFiles(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		rows, err := db.Query("SELECT id, filename, size, kind, created_at FROM files ORDER BY created_at DESC")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()

		files := []File{}
		for rows.Next() {
			var f File
			if err := rows.Scan(&f.ID, &f.FileName, &f.Size, &f.Kind, &f.CreatedAt); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			files = append(files, f)
		}
		if err := rows.Err(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"files": files})
	}
}

func getFileByID(db *sql.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")

		var f File
		err := db.QueryRow(
			"SELECT id, filename, size, kind, created_at FROM files WHERE id = ?",
			id,
		).Scan(&f.ID, &f.FileName, &f.Size, &f.Kind, &f.CreatedAt)

		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, f)
	}
}

// postCompress accepts the upload and queues it. It answers 202 immediately —
// the client polls /api/status/:id for the rest.
func postCompress(db *sql.DB, q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		file, err := c.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		presetKey := c.PostForm("preset")
		if _, ok := job.LookupPreset(presetKey); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown preset %q", presetKey)})
			return
		}

		fileID := job.NewID()
		// The uploaded name is attacker-controlled: keep only the base name so
		// it can't escape the upload directory.
		origName := filepath.Base(file.Filename)
		savedPath := filepath.Join(uploadDir, fileID+"-"+sanitize(origName))
		if err := c.SaveUploadedFile(file, savedPath); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if _, err := db.Exec(
			`INSERT INTO files (id, filename, size, path, kind) VALUES (?, ?, ?, ?, 'source')`,
			fileID, origName, file.Size, savedPath,
		); err != nil {
			os.Remove(savedPath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		jobID := job.NewID()
		if _, err := db.Exec(
			`INSERT INTO jobs (id, file_id, status, preset) VALUES (?, ?, ?, ?)`,
			jobID, fileID, job.StatusQueued, presetKey,
		); err != nil {
			os.Remove(savedPath)
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		err = q.Submit(job.Job{
			ID:       jobID,
			FileID:   fileID,
			Path:     savedPath,
			OrigName: origName,
			Preset:   presetKey,
		})
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusAccepted, gin.H{"job_id": jobID}) // 202: work is queued, not done
	}
}

func getJobStatus(q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		status, ok := q.Status(c.Param("id"))
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusOK, status)
	}
}

// getJobs lists every job, newest first. The UI renders its three sections
// from this one response.
func getJobs(q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		jobs, err := q.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, jobs)
	}
}

// postRetry re-runs a failed job whose source upload is still on disk.
func postRetry(q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")

		switch err := q.Retry(id); {
		case err == nil:
			c.JSON(http.StatusAccepted, gin.H{"job_id": id})
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		case errors.Is(err, job.ErrJobActive):
			c.JSON(http.StatusConflict, gin.H{"error": "job is already running"})
		case errors.Is(err, job.ErrSourceGone):
			c.JSON(http.StatusConflict, gin.H{"error": "original upload is no longer available"})
		case errors.Is(err, job.ErrQueueFull):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
	}
}

// deleteJob removes a job along with its upload and compressed output.
func deleteJob(q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch err := q.Delete(c.Param("id")); {
		case err == nil:
			c.Status(http.StatusNoContent)
		case errors.Is(err, sql.ErrNoRows):
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		case errors.Is(err, job.ErrJobActive):
			c.JSON(http.StatusConflict, gin.H{"error": "job is still running"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
	}
}

// getDownload streams the compressed result under its stored filename.
func getDownload(q *job.Queue) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := q.Output(c.Param("id"))
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "no compressed file for this job"})
			return
		}
		if _, err := os.Stat(out.Path); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "compressed file is missing"})
			return
		}

		c.FileAttachment(out.Path, out.Filename)
	}
}

// sanitize strips path separators and other characters that make for awkward
// filenames on disk.
func sanitize(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '_'
		case r < 0x20:
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "upload"
	}
	return name
}

func main() {
	for _, dir := range []string{uploadDir, outputDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatal(err)
		}
	}

	db, err := database.Connect()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := database.CreateTables(db); err != nil {
		log.Fatal(err)
	}

	queue := job.NewQueue(db, outputDir, queueBuffer, workers)
	log.Printf("using encoder: %s", queue.Encoder().Display)

	router := gin.Default()
	router.MaxMultipartMemory = 32 << 20 // buffer in memory before spilling to disk
	router.Use(limitUploadSize(maxUpload))

	router.StaticFile("/", filepath.Join(staticDir, "index.html"))
	router.Static("/static", staticDir)

	api := router.Group("/api")
	{
		api.GET("/presets", getPresets)
		api.POST("/compress", postCompress(db, queue))
		api.GET("/status/:id", getJobStatus(queue))
		api.GET("/download/:id", getDownload(queue))

		api.GET("/jobs", getJobs(queue))
		api.POST("/jobs/:id/retry", postRetry(queue))
		api.DELETE("/jobs/:id", deleteJob(queue))

		api.GET("/files", getFiles(db))
		api.GET("/files/:id", getFileByID(db))
	}

	addr := defaultListenAddr
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		addr = v
	}

	log.Printf("listening on http://%s", addr)
	if err := router.Run(addr); err != nil {
		log.Fatal(err)
	}
}

// limitUploadSize caps request bodies so a huge POST can't fill the disk.
func limitUploadSize(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		c.Next()
	}
}
