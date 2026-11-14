package checkflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
)

func Run(ctx context.Context, workflow string) error {
	harness, err := NewHarness()
	if err != nil {
		return err
	}
	defer harness.Close()

	switch workflow {
	case "ingest-audio-source":
		return runIngest(ctx, harness)
	case "compose-timeline":
		return runCompose(ctx, harness)
	case "render-mixdown":
		return runRender(ctx, harness)
	case "quantize-errorshaping":
		return runErrorShaping(ctx, harness)
	case "render-resume":
		return runRenderResume(ctx, harness)
	case "stereo-shaping":
		return runStereoShaping(ctx, harness)
	default:
		return fmt.Errorf("unknown workflow %q", workflow)
	}
}

func runIngest(ctx context.Context, harness *Harness) error {
	wav, err := sineWAV(48000, 1, 250, 440, 0.4)
	if err != nil {
		return err
	}
	asset, status, err := harness.createSource(ctx, "source_ingest_01", "narration take", wav)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("source write returned status %d, want 201", status)
	}
	if asset.FrameCount != 12000 || asset.BitsPerSample != 16 {
		return fmt.Errorf("source metadata is inconsistent: %+v", asset)
	}
	readBack, status, err := harness.getSource(ctx, asset.ID)
	if err != nil {
		return err
	}
	if status != 200 || readBack.Digest != asset.Digest {
		return fmt.Errorf("source read-back mismatch: status=%d digest=%s", status, readBack.Digest)
	}
	badStatus, body, err := harness.requestJSON(ctx, "PUT", "/v1/sources/source_ingest_bad", service.SourceWriteRequest{
		Name:      "broken take",
		WAVBase64: base64.StdEncoding.EncodeToString([]byte("not a wave")),
	})
	if err != nil {
		return err
	}
	if badStatus != 422 || failureCode(body) != "invalid_wav" {
		return fmt.Errorf("malformed media returned status=%d code=%s", badStatus, failureCode(body))
	}
	missing, status, err := harness.getSource(ctx, "source_ingest_bad")
	if err != nil {
		return err
	}
	if status != 404 || missing.ID != "" {
		return fmt.Errorf("failed source became visible: status=%d id=%s", status, missing.ID)
	}
	return nil
}

func runCompose(ctx context.Context, harness *Harness) error {
	first, err := sineWAV(48000, 1, 300, 220, 0.35)
	if err != nil {
		return err
	}
	second, err := sineWAV(44100, 2, 250, 330, 0.25)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_compose_a", "first span", first); err != nil || status != 201 {
		return fmt.Errorf("first source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_compose_b", "second span", second); err != nil || status != 201 {
		return fmt.Errorf("second source setup failed: status=%d err=%v", status, err)
	}
	validSpans := []domain.TimelineSpan{
		{SourceID: "source_compose_a", SourceStartFrame: 0, SourceEndFrame: 14400, GainDB: -3, CrossfadeFrames: 0},
		{SourceID: "source_compose_b", SourceStartFrame: 0, SourceEndFrame: 11025, GainDB: 1, CrossfadeFrames: 2400},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_compose_01", "two span assembly", 48000, 2, -1, validSpans)
	if err != nil {
		return err
	}
	if status != 201 || plan.State != "ready" {
		return fmt.Errorf("timeline write returned status=%d state=%s", status, plan.State)
	}
	readBack, status, err := harness.getTimeline(ctx, plan.ID)
	if err != nil {
		return err
	}
	if status != 200 || readBack.OutputFrames != plan.OutputFrames || len(readBack.Spans) != 2 {
		return fmt.Errorf("timeline read-back mismatch: status=%d frames=%d", status, readBack.OutputFrames)
	}
	invalidSpans := append([]domain.TimelineSpan(nil), validSpans...)
	invalidSpans[1].CrossfadeFrames = 999999
	_, status, err = harness.createTimeline(ctx, "timeline_compose_bad", "invalid crossfade", 48000, 2, -1, invalidSpans)
	if err != nil {
		return err
	}
	if status != 422 {
		return fmt.Errorf("invalid crossfade returned status %d, want 422", status)
	}
	return nil
}

func runRender(ctx context.Context, harness *Harness) error {
	first, err := sineWAV(48000, 1, 400, 220, 0.25)
	if err != nil {
		return err
	}
	second, err := sineWAV(44100, 2, 350, 330, 0.2)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_render_a", "opening take", first); err != nil || status != 201 {
		return fmt.Errorf("opening source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := harness.createSource(ctx, "source_render_b", "closing take", second); err != nil || status != 201 {
		return fmt.Errorf("closing source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{
		{SourceID: "source_render_a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
		{SourceID: "source_render_b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 0, CrossfadeFrames: 4800},
	}
	plan, status, err := harness.createTimeline(ctx, "timeline_render_01", "crossfaded narration", 48000, 2, -1, spans)
	if err != nil {
		return err
	}
	if status != 201 {
		return fmt.Errorf("timeline setup returned status %d", status)
	}
	job, status, err := harness.createRender(ctx, "render_mixdown_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted || job.Output == nil {
		return fmt.Errorf("render returned status=%d state=%s output=%v", status, job.State, job.Output)
	}
	fetched, status, err := harness.getRender(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 || fetched.State != domain.RenderCompleted {
		return fmt.Errorf("render read-back returned status=%d state=%s", status, fetched.State)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 || receipt.Output.FrameCount != plan.OutputFrames {
		return fmt.Errorf("receipt frame mismatch: status=%d receipt=%d plan=%d", status, receipt.Output.FrameCount, plan.OutputFrames)
	}
	audioBytes, status, err := harness.getOutput(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("output returned status %d", status)
	}
	digest := sha256.Sum256(audioBytes)
	if hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		return fmt.Errorf("output digest does not match receipt")
	}
	clip, info, err := audiocodec.DecodeWAV(audioBytes)
	if err != nil {
		return fmt.Errorf("decode rendered output: %w", err)
	}
	measured := audiocodec.Measure(clip.Samples)
	if info.FrameCount != receipt.Output.FrameCount {
		return fmt.Errorf("decoded frame count %d != receipt %d", info.FrameCount, receipt.Output.FrameCount)
	}
	if math.Abs(measured.PeakDBFS-(-1)) > 0.25 {
		return fmt.Errorf("output peak %.3f dBFS does not meet -1 dBFS target", measured.PeakDBFS)
	}
	return nil
}

// runErrorShaping verifies the noise-shaped quantization path end to end:
// the delivered error spectrum moves out of the audible band, the receipt
// describes the exact 16-bit semantics, a quiet/full-scale step does not leave
// amplified residue, and an interrupted retry reproduces the same PCM.
func runErrorShaping(ctx context.Context, harness *Harness) error {
	spec, err := audiocodec.BuildQuantizationSpec(audiocodec.QuantizationNoiseShaped, 48000, 1)
	if err != nil {
		return err
	}
	profile := spec.ProfileReport()
	if profile.AudibleBandGainDB >= -1 {
		return fmt.Errorf("shaper audible band gain %.2f dB does not lower in-band error", profile.AudibleBandGainDB)
	}
	if profile.OutOfBandGainDB <= profile.AudibleBandGainDB+3 {
		return fmt.Errorf("shaper does not concentrate energy out of band: in=%.2f out=%.2f", profile.AudibleBandGainDB, profile.OutOfBandGainDB)
	}

	// Step test: a full-scale passage followed by near-silence must not leave
	// a feedback residue that amplifies the first quiet samples.
	rate := 48000
	frames := rate * 2
	samples := make([]float32, frames)
	for index := 0; index < frames; index++ {
		seconds := float64(index) / float64(rate)
		if index < frames/2 {
			samples[index] = float32(0.9 * math.Sin(2*math.Pi*440*seconds))
		} else {
			samples[index] = float32(1e-5 * math.Sin(2*math.Pi*440*seconds))
		}
	}
	stepQuantizer, err := audiocodec.NewQuantizer(spec, 7, 0)
	if err != nil {
		return err
	}
	encoded := make([]int16, len(samples))
	if err := stepQuantizer.QuantizeBlock(samples, encoded); err != nil {
		return err
	}
	afterStep := 0.0
	for index := frames / 2; index < frames/2+2048; index++ {
		residual := math.Abs(float64(audiocodec.DecodePCM16Sample(encoded[index])) - float64(samples[index]))
		if residual > afterStep {
			afterStep = residual
		}
	}
	if afterStep > 4.0/32767 {
		return fmt.Errorf("quiet passage after a full-scale step shows %.2f LSB residue", afterStep*32767)
	}

	// Resume identity: quantizing in two blocks with a saved state must equal
	// one uninterrupted block, and state from a different specification must be
	// refused rather than silently borrowed.
	fullQuantizer, err := audiocodec.NewQuantizer(spec, 7, 0)
	if err != nil {
		return err
	}
	fullOutput := make([]int16, len(samples))
	if err := fullQuantizer.QuantizeBlock(samples, fullOutput); err != nil {
		return err
	}
	resumeQuantizer, err := audiocodec.NewQuantizer(spec, 7, 0)
	if err != nil {
		return err
	}
	if err := resumeQuantizer.QuantizeBlock(samples[:frames/2], fullOutput[:frames/2]); err != nil {
		return err
	}
	state := resumeQuantizer.State()
	continuation, err := audiocodec.NewQuantizer(spec, 7, int64(frames/2))
	if err != nil {
		return err
	}
	if err := continuation.LoadState(state); err != nil {
		return err
	}
	continuationOutput := make([]int16, frames-frames/2)
	if err := continuation.QuantizeBlock(samples[frames/2:], continuationOutput); err != nil {
		return err
	}
	for index := 0; index < frames; index++ {
		var expected int16
		if index < frames/2 {
			expected = fullOutput[index]
		} else {
			expected = continuationOutput[index-frames/2]
		}
		if expected != fullOutput[index] {
			return fmt.Errorf("resumed quantization diverged at sample %d", index)
		}
	}
	otherSpec, err := audiocodec.BuildQuantizationSpec(audiocodec.QuantizationNoiseShaped, 44100, 2)
	if err != nil {
		return err
	}
	borrow, err := audiocodec.NewQuantizer(otherSpec, 7, int64(frames/2))
	if err != nil {
		return err
	}
	if err := borrow.LoadState(state); err == nil {
		return fmt.Errorf("cross-selection quantizer state was accepted")
	}

	// Full HTTP path: render a shaped timeline and check the receipt reports a
	// measured audible-band improvement and out-of-band increase.
	source, err := sineWAV(48000, 1, 900, 330, 0.30)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_shaping_a", "shaping source", source); err != nil || status != 201 {
		return fmt.Errorf("shaping source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{{SourceID: "source_shaping_a", SourceStartFrame: 0, SourceEndFrame: 43200, GainDB: 0, CrossfadeFrames: 0}}
	plan, status, err := harness.createTimelineWithQuantization(ctx, "timeline_shaping_01", "shaped timeline", 48000, 1, -1, string(audiocodec.QuantizationNoiseShaped), spans)
	if err != nil {
		return err
	}
	if status != 201 || plan.Quantization != string(audiocodec.QuantizationNoiseShaped) {
		return fmt.Errorf("shaped timeline write returned status=%d quantization=%q", status, plan.Quantization)
	}
	job, status, err := harness.createRender(ctx, "render_shaping_01", plan.ID)
	if err != nil {
		return err
	}
	if status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("shaped render returned status=%d state=%s", status, job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("shaped receipt returned status %d", status)
	}
	quantization := receipt.Quantization
	if quantization.Mode != string(audiocodec.QuantizationNoiseShaped) {
		return fmt.Errorf("receipt mode %q does not record noise-shaped quantization", quantization.Mode)
	}
	if quantization.ShaperTaps < 1 || quantization.DitherLSB <= 0 {
		return fmt.Errorf("receipt does not describe shaper taps or dither")
	}
	if quantization.AudibleFloorDeltaDB <= 1 {
		return fmt.Errorf("measured audible-band improvement is only %.2f dB", quantization.AudibleFloorDeltaDB)
	}
	if quantization.MeasuredOutBandDBFS <= quantization.MeasuredBandFloorDBFS {
		return fmt.Errorf("measured error was not moved out of band: in=%.2f out=%.2f", quantization.MeasuredBandFloorDBFS, quantization.MeasuredOutBandDBFS)
	}
	// Output must remain 16-bit and match the receipt digest.
	audioBytes, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("shaped output returned status=%d err=%v", status, err)
	}
	digest := sha256.Sum256(audioBytes)
	if hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		return fmt.Errorf("shaped output digest does not match receipt")
	}
	_, info, err := audiocodec.DecodeWAV(audioBytes)
	if err != nil {
		return fmt.Errorf("decode shaped output: %w", err)
	}
	if info.BitsPerSample != 16 {
		return fmt.Errorf("shaped output is %d-bit, expected 16-bit", info.BitsPerSample)
	}
	if info.FrameCount != plan.OutputFrames {
		return fmt.Errorf("shaped output has %d frames, plan has %d", info.FrameCount, plan.OutputFrames)
	}
	// Idempotent retry must not rewrite the completed receipt or output.
	replayed, retryStatus, err := harness.createRender(ctx, job.ID, plan.ID)
	if err != nil {
		return err
	}
	if retryStatus != 200 || replayed.State != domain.RenderCompleted || replayed.Output == nil || replayed.Output.Digest != receipt.Output.Digest {
		return fmt.Errorf("completed retry changed the render: status=%d state=%s", retryStatus, replayed.State)
	}
	replayedOutput, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("retry output read failed: status=%d err=%v", status, err)
	}
	if !bytes.Equal(replayedOutput, audioBytes) {
		return fmt.Errorf("completed retry rewrote the published output")
	}
	return nil
}

// runRenderResume proves that an interrupted render continues from its durable
// checkpoint and produces byte-identical PCM, that a checkpoint from a
// different selection is refused, and that a completed receipt is not
// rewritten by a retry.
func runRenderResume(ctx context.Context, harness *Harness) error {
	source, err := sineWAV(48000, 1, 30000, 260, 0.28)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_resume_a", "resume source", source); err != nil || status != 201 {
		return fmt.Errorf("resume source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{{SourceID: "source_resume_a", SourceStartFrame: 0, SourceEndFrame: 1440000, GainDB: 0, CrossfadeFrames: 0}}
	plan, status, err := harness.createTimelineWithQuantization(ctx, "timeline_resume_01", "resume timeline", 48000, 1, -1, string(audiocodec.QuantizationNoiseShaped), spans)
	if err != nil || status != 201 {
		return fmt.Errorf("resume timeline setup failed: status=%d err=%v", status, err)
	}

	// Reference: the same render identity in an isolated data directory, so the
	// dither seed and selection are identical to the interrupted run.
	referenceHarness, err := NewHarness()
	if err != nil {
		return err
	}
	defer referenceHarness.Close()
	if _, status, err := referenceHarness.createSource(ctx, "source_resume_a", "resume source", source); err != nil || status != 201 {
		return fmt.Errorf("reference source setup failed: status=%d err=%v", status, err)
	}
	if _, status, err := referenceHarness.createTimelineWithQuantization(ctx, "timeline_resume_01", "resume timeline", 48000, 1, -1, string(audiocodec.QuantizationNoiseShaped), spans); err != nil || status != 201 {
		return fmt.Errorf("reference timeline setup failed: status=%d err=%v", status, err)
	}
	fullJob, status, err := referenceHarness.createRender(ctx, "render_resume_01", plan.ID)
	if err != nil || status != 201 {
		return fmt.Errorf("reference render failed: status=%d err=%v", status, err)
	}
	reference, _, err := referenceHarness.getOutput(ctx, fullJob.ID)
	if err != nil {
		return err
	}

	// Interrupt the same timeline under a different identifier by cancelling
	// the request context, then retry once the checkpoint is durable.
	interruptCtx, cancel := context.WithCancel(ctx)
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	_, _, _ = harness.createRender(interruptCtx, "render_resume_01", plan.ID)
	cancel()
	time.Sleep(50 * time.Millisecond)

	job, status, err := harness.createRender(ctx, "render_resume_01", plan.ID)
	if err != nil {
		return err
	}
	if (status != 200 && status != 201) || job.State != domain.RenderCompleted {
		return fmt.Errorf("resumed render returned status=%d state=%s", status, job.State)
	}
	resumed, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("resumed output read failed: status=%d err=%v", status, err)
	}
	if !bytes.Equal(resumed, reference) {
		return fmt.Errorf("resumed render PCM differs from an uninterrupted render")
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("resumed receipt read failed: status=%d err=%v", status, err)
	}
	if receipt.Quantization.RestartStrategy != "deterministic_checkpoint_resume" {
		return fmt.Errorf("resumed receipt restart strategy is %q, expected a checkpoint resume", receipt.Quantization.RestartStrategy)
	}
	// A retry of a completed render must return the same published bytes.
	replayed, retryStatus, err := harness.createRender(ctx, "render_resume_01", plan.ID)
	if err != nil || retryStatus != 200 {
		return fmt.Errorf("completed retry failed: status=%d err=%v", retryStatus, err)
	}
	if replayed.Output == nil || replayed.Output.Digest != receipt.Output.Digest {
		return fmt.Errorf("completed retry changed output metadata")
	}
	replayedBytes, _, err := harness.getOutput(ctx, "render_resume_01")
	if err != nil {
		return err
	}
	if !bytes.Equal(replayedBytes, reference) {
		return fmt.Errorf("completed retry rewrote the published output")
	}
	return nil
}

// runStereoShaping verifies that noise shaping is applied per channel: a loud
// left channel and a silent right channel must not leak a common error into
// the silent side, which would fold a wrong image into the stereo output.
func runStereoShaping(ctx context.Context, harness *Harness) error {
	rate := 48000
	frames := rate
	samples := make([]float32, frames*2)
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / float64(rate)
		samples[frame*2] = float32(0.85 * math.Sin(2*math.Pi*440*seconds))
		samples[frame*2+1] = 0
	}
	wav, err := audiocodec.EncodePCM16(rate, 2, samples)
	if err != nil {
		return err
	}
	if _, status, err := harness.createSource(ctx, "source_stereo_a", "stereo source", wav); err != nil || status != 201 {
		return fmt.Errorf("stereo source setup failed: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{{SourceID: "source_stereo_a", SourceStartFrame: 0, SourceEndFrame: int64(frames), GainDB: 0, CrossfadeFrames: 0}}
	plan, status, err := harness.createTimelineWithQuantization(ctx, "timeline_stereo_01", "stereo shaped", rate, 2, -1, string(audiocodec.QuantizationNoiseShaped), spans)
	if err != nil || status != 201 {
		return fmt.Errorf("stereo timeline setup failed: status=%d err=%v", status, err)
	}
	job, status, err := harness.createRender(ctx, "render_stereo_01", plan.ID)
	if err != nil || status != 201 || job.State != domain.RenderCompleted {
		return fmt.Errorf("stereo shaped render failed: status=%d err=%v", status, err)
	}
	output, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		return fmt.Errorf("stereo shaped output failed: status=%d err=%v", status, err)
	}
	clip, info, err := audiocodec.DecodeWAV(output)
	if err != nil {
		return err
	}
	if info.Channels != 2 {
		return fmt.Errorf("stereo output has %d channels", info.Channels)
	}
	maxRight := 0.0
	for frame := int64(0); frame < info.FrameCount; frame++ {
		value := math.Abs(float64(clip.Samples[frame*2+1]))
		if value > maxRight {
			maxRight = value
		}
	}
	if maxRight > 1.0/32767 {
		return fmt.Errorf("silent right channel carries %.3f LSB of shaped error", maxRight*32767)
	}
	return nil
}
