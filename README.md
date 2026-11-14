# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades.
- Peak normalization into a requested dBFS target.
- Optional error-shaped 16-bit quantization that keeps the 16-bit container.
- Bounded streaming renders with durable checkpoints and idempotent resume.
- Durable render states, receipts, and atomically committed output folders.

Automated tests are intentionally deferred. A later task stage will add unit,
integration, and end-to-end verification around the production interfaces.

## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

Response:

```json
{
  "id": "opening_take_01",
  "name": "opening take",
  "digest": "b7c0...",
  "byte_count": 96044,
  "sample_rate": 48000,
  "channels": 1,
  "bits_per_sample": 16,
  "frame_count": 48000,
  "duration_ms": 1000,
  "peak_dbfs": -8.0,
  "rms_dbfs": -11.0,
  "created_at": "2026-11-09T12:00:00Z"
}
```

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "quantization": "noise_shaped_v1",
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

Timelines may select a `quantization` mode. The default, `legacy`, keeps the
original 16-bit round-to-nearest encoding and produces byte-identical output to
earlier releases. `noise_shaped_v1` keeps the same 16-bit PCM container but
moves quantization error out of the audible band with deterministic error
feedback plus shaped TPDF dither. The mode is recorded on the timeline so it is
part of the render's selection digest.

## Optional error-shaped 16-bit quantization

`noise_shaped_v1` is intended for the long-tail problem where a quiet passage
carries audible quantization spurs even though the loud body of the file is
fine. It does not raise the bit depth, relax a noise budget, or substitute a
random sequence for a real shaper:

- A fixed 33-tap noise-transfer function is designed from a least-squares
  objective over the audible band (20 Hz to `min(16 kHz, 0.85 * Nyquist)`).
  The design depends only on the sample rate, so a retried render derives the
  same taps.
- The loop feeds back `y[n] - base[n]`, so the delivered error spectrum is
  exactly `C(z)` times the quantizer's (dither + rounding) error. The in-band
  error power falls by `|C|^2`; the same power appears above the band edge.
  Nothing is hidden — the receipt reports both band floors.
- The single-bit container stays 16-bit, and the normalization gain still
  guarantees the output never clips, so the reported peak remains truthful.
- Each decision retains at most one LSB of residual history. A sudden step
  between a full-scale passage and near silence cannot amplify stale state,
  because a saturated decision discards its residual and the feedback sum is
  clamped.
- Stereo is shaped per channel with the channel index mixed into the
  deterministic dither, so a common error path is not imposed across channels.

The receipt's `quantization` block records the mode, the specification digest,
the shaper profile, tap count, dither amplitude, audible-band edges, the
deterministic `audible_band_gain_db` / `out_of_band_gain_db`, and the measured
audible and out-of-band error floors of the delivered WAV. It also reports
`audible_floor_delta_db`, the measured improvement against what plain
round-to-nearest would have produced from the same normalized samples.

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed render records a
terminal `failed` state with a stable error code and does not expose a partial
output folder.

Renders stream in bounded blocks: sources are read through a small rolling
window, the mix is normalized from a first pass over the same blocks, and the
WAV is written block by block. No render holds a whole decoded source or a
whole mix in memory, so long projects stay inside a fixed working set.

Every render also persists a checkpoint (quantizer error history, measurement
accumulator, normalization gain, and frame index) beside the partial WAV.
If a request is interrupted, repeating the same render identifier continues
from that checkpoint and produces the same PCM as an uninterrupted render.
The checkpoint is bound to a selection digest that covers the timeline spans,
source digests, target peak, quantization specification, and dither seed; a
checkpoint from any other selection is refused rather than borrowing its error
history. A `completed` render is terminal and a retry returns the unchanged
receipt and output bytes.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav
    receipt.json
  tmp/
```

Source and render folders are assembled in `tmp/` and moved into place with one
directory rename. Metadata updates use same-directory temporary files followed
by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/render/        timeline preparation, mixing, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
go run ./cmd/timelinecheck -workflow quantize-errorshaping
go run ./cmd/timelinecheck -workflow render-resume
go run ./cmd/timelinecheck -workflow stereo-shaping
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered output.
The checks are bounded and do not require network access. `quantize-errorshaping`
asserts that the shaper moves error out of the audible band and that the receipt
records a measured improvement; `render-resume` interrupts a render and asserts
that a retry reproduces the uninterrupted PCM, and `stereo-shaping` asserts a
silent channel stays silent under per-channel shaping.

## Deferred test boundary

This baseline contains no unit tests, test fixtures, browser checks, or
`test_command`. The downstream task stage owns test creation and red/green
verification. It should extend coverage around WAV chunk edge cases, timeline
crossfade bounds, failed job cleanup, concurrent idempotency, and HTTP error
mapping.
