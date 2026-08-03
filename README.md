# go-video-compressor

A small self-hosted service for compressing videos over a local network. Drop a file in the
browser, pick a quality preset, and get an H.264 MP4 back — encoded on the GPU when one is
available.

Built for the case where the machine with the fast GPU is not the machine with the video.

## How it works

Uploads are queued and encoded one at a time by a background worker. The browser polls for
progress, which is parsed live from ffmpeg's own reporting, and every job survives a reload or a
restart because its state lives in SQLite.

The UI has three lists: **in progress**, **failed**, and **success**. Failed jobs keep their
upload so they can be retried; successful ones drop it, since the compressed file is what you came
for.

## Requirements

- **Go 1.26+**
- **ffmpeg and ffprobe** on `PATH` — the service shells out to both
- **A C compiler** — the SQLite driver ([mattn/go-sqlite3]) uses cgo, so `CGO_ENABLED=0` will not
  build
- *Optional:* an NVIDIA GPU with NVENC support

The encoder is chosen at startup by running a throwaway encode: if `h264_nvenc` actually works it
is used, otherwise the service falls back to `libx264` on the CPU. Listing an encoder in
`ffmpeg -encoders` is not enough — NVENC is compiled into most ffmpeg builds even on machines with
no NVIDIA driver — so the probe is a real encode. The chosen encoder is logged on startup and
shown next to each running job.

## Running

```sh
go run .
```

Then open <http://localhost:8080>.

Run it **from the project root**: the database, `uploads/`, `outputs/`, and `static/` are all
resolved relative to the working directory.

The service binds `0.0.0.0:8080` so other machines on the network can reach it. Override with
`LISTEN_ADDR`:

```sh
LISTEN_ADDR=127.0.0.1:9000 go run .   # localhost only
```

Set `GIN_MODE=release` to drop gin's debug output and startup warnings. Per-request logging stays
on either way.

Request bodies are capped at 8 GiB.

## Presets

| Key        | Label        | Resolution     | Quality (CRF/CQ) | Audio |
| ---------- | ------------ | -------------- | ---------------- | ----- |
| `quality`  | High quality | up to 1080p    | 21               | 160k  |
| `balanced` | Balanced     | up to 1080p    | 26               | 128k  |
| `small`    | Small        | up to 720p     | 30               | 96k   |

Videos are only ever scaled **down** — a 720p source stays 720p under the 1080p presets. Quality
is constant-rate, so the output size depends on the content rather than a fixed bitrate; the UI
shows a running estimate extrapolated from bytes written so far.

Presets live in [`job/preset.go`](job/preset.go) and are served to the frontend, so adding one is
a single struct literal.

## API

| Method   | Path                    | Description                                                  |
| -------- | ----------------------- | ------------------------------------------------------------ |
| `GET`    | `/api/presets`          | Available compression presets, in display order              |
| `POST`   | `/api/compress`         | Multipart upload (`file`, `preset`) → `202 {"job_id": "..."}` |
| `GET`    | `/api/jobs`             | Every job, newest first, with live progress                  |
| `GET`    | `/api/status/:id`       | One job's status                                             |
| `POST`   | `/api/jobs/:id/retry`   | Re-run a failed job under the same ID                        |
| `DELETE` | `/api/jobs/:id`         | Delete a job and every file it owns                          |
| `GET`    | `/api/download/:id`     | Download the compressed result                               |
| `GET`    | `/api/files`            | File registry (debugging)                                    |

A job moves through `queued → analyzing → encoding → done`, or lands on `error`. Retry and delete
return `409` while a job is still running, and retry also returns `409` once the original upload
is gone. The queue holds 10 pending jobs; past that, uploads are rejected with `503` rather than
left hanging.

## Data model

`files` is a registry of everything on disk — each row is either a `source` upload or a
`compressed` output, with its size and path. `jobs` links the two ends via `file_id` and
`output_file_id`.

When a job succeeds, the upload is deleted and its row's path is cleared, keeping the original
filename and size for display. Deleting a job removes both files and both rows. Schema changes are
applied on startup as additive column migrations, so an existing database is upgraded in place.

## Layout

```
main.go            HTTP handlers and routing
job/               queue, worker, ffmpeg integration, presets, progress tracking
database/          connection, schema, migrations
static/index.html  the entire frontend, no build step
uploads/           incoming files (gitignored)
outputs/           compressed results (gitignored)
```

## A note on security

There is **no authentication**. Anyone who can reach the port can upload files, download results,
and delete jobs — and uploaded files are handed to ffmpeg. Run it on a network you trust, or bind
it to localhost and reach it over SSH. Do not expose it to the internet.

Compressed outputs are never pruned automatically; they stay until deleted from the UI.

[mattn/go-sqlite3]: https://github.com/mattn/go-sqlite3
