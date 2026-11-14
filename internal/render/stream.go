package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// SourceFile identifies an immutable source WAV that the renderer streams.
type SourceFile struct {
	Asset domain.SourceAsset
	Path  string
}

// AudioSource opens immutable sources for streaming reads.
type AudioSource interface {
	OpenSource(ctx context.Context, sourceID string) (SourceFile, error)
	OpenReader(ctx context.Context, file SourceFile) (audiocodec.FrameReader, error)
}

// StreamLimits bounds a streaming render.
type StreamLimits struct {
	MaxOutputFrames int64
	BlockFrames     int
}

// QuantizationRequest selects the PCM encoding policy and the restart point.
type QuantizationRequest struct {
	Mode            audiocodec.QuantizationMode
	Seed            uint64
	SelectionDigest string
	Checkpoint      *RenderCheckpoint
}

// RenderCheckpoint is the durable continuation of one render. It binds the
// quantizer state to the exact source selection and quantization
// specification, so a resumed run can never inherit error history produced for
// a different selection.
type RenderCheckpoint struct {
	SelectionDigest string                    `json:"selection_digest"`
	QuantizerDigest string                    `json:"quantizer_digest"`
	FrameIndex      int64                     `json:"frame_index"`
	StartFrame      int64                     `json:"start_frame"`
	Peak            float64                   `json:"peak"`
	Gain            float64                   `json:"gain"`
	Quantizer       audiocodec.QuantizerState `json:"quantizer"`
	Measurement     MeasurementState          `json:"measurement"`
	OutputPeak      float64                   `json:"output_peak"`
	OutputSquared   float64                   `json:"output_squared"`
	OutputSamples   int64                     `json:"output_samples"`
}

// StreamResult describes a completed streaming render.
type StreamResult struct {
	OutputPath string
	Output     domain.OutputAsset
	Receipt    domain.RenderReceipt
	FinalState RenderCheckpoint
}

const (
	defaultBlockFrames       = 1024
	checkpointIntervalFrames = 48000
)

// SelectionDigest binds a render to every input that can change its PCM:
// render identity, span selection, source digests, sample layout, target
// peak, quantization mode, and dither seed.
func SelectionDigest(renderID string, plan domain.Timeline, sourceDigests map[string]string, mode audiocodec.QuantizationMode, seed uint64) string {
	type spanBinding struct {
		Span   domain.TimelineSpan `json:"span"`
		Digest string              `json:"digest"`
	}
	bindings := make([]spanBinding, len(plan.Spans))
	for index, span := range plan.Spans {
		bindings[index] = spanBinding{Span: span, Digest: sourceDigests[span.SourceID]}
	}
	payload := struct {
		RenderID     string        `json:"render_id"`
		TimelineID   string        `json:"timeline_id"`
		SampleRate   int           `json:"sample_rate"`
		Channels     int           `json:"channels"`
		TargetPeak   float64       `json:"target_peak_dbfs"`
		Quantization string        `json:"quantization"`
		Seed         uint64        `json:"seed"`
		Bindings     []spanBinding `json:"bindings"`
	}{
		RenderID:     renderID,
		TimelineID:   plan.ID,
		SampleRate:   plan.SampleRate,
		Channels:     plan.OutputChannels,
		TargetPeak:   plan.TargetPeakDBFS,
		Quantization: string(mode),
		Seed:         seed,
		Bindings:     bindings,
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

type channelRemap int

const (
	remapNone channelRemap = iota
	remapMonoToStereo
	remapStereoToMono
)

type streamSpan struct {
	Index            int
	SourceID         string
	SourceDigest     string
	SourceStartFrame int64
	SourceEndFrame   int64
	SourceRate       int
	SourceChannels   int
	OutputStartFrame int64
	OutputEndFrame   int64
	OutputFrames     int64
	CrossfadeFrames  int64
	GainDB           float64
	ChannelAction    string
	file             SourceFile
	reader           audiocodec.FrameReader
	gain             float32
	remap            channelRemap
	step             float64
	sliced           bool
	sliceFrames      int64
}

type streamEngine struct {
	ctx            context.Context
	renderID       string
	plan           domain.Timeline
	sources        AudioSource
	limits         StreamLimits
	spans          []streamSpan
	frames         int64
	channels       int
	sampleRate     int
	blockFrames    int
	quantizer      *audiocodec.Quantizer
	quantSpec      audiocodec.QuantizationSpec
	startFrame     int64
	selection      string
	measurement    *measurementAccumulator
	scratch        []float32
	checkpointSink func(RenderCheckpoint) error
	normPeak       float64
	normGain       float64
	outputPeak     float64
	outputSquared  float64
	outputSamples  int64
}

// RenderStream writes the normalized PCM16 WAV to outputPath, resuming from
// request.Checkpoint when it is present, and returns a receipt measured from
// exactly those bytes.
func RenderStream(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	request QuantizationRequest,
	sources AudioSource,
	limits StreamLimits,
	outputPath string,
	checkpointSink func(RenderCheckpoint) error,
) (StreamResult, error) {
	if outputPath == "" {
		return StreamResult{}, errors.New("render requires an output path")
	}
	engine, err := prepareStreamEngine(ctx, renderID, plan, request, sources, limits)
	if err != nil {
		return StreamResult{}, err
	}
	defer engine.close()
	engine.checkpointSink = checkpointSink
	return engine.render(outputPath)
}

func prepareStreamEngine(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	request QuantizationRequest,
	sources AudioSource,
	limits StreamLimits,
) (*streamEngine, error) {
	if sources == nil {
		return nil, errors.New("render requires a source provider")
	}
	if plan.SampleRate < 8000 || plan.SampleRate > 192000 {
		return nil, errors.New("timeline sample rate is outside the accepted range")
	}
	if plan.OutputChannels != 1 && plan.OutputChannels != 2 {
		return nil, errors.New("timeline output channels must be one or two")
	}
	mode := request.Mode
	if mode == "" {
		mode = audiocodec.QuantizationLegacy
	}
	quantSpec, err := audiocodec.BuildQuantizationSpec(mode, plan.SampleRate, plan.OutputChannels)
	if err != nil {
		return nil, err
	}
	if request.SelectionDigest == "" {
		return nil, errors.New("render request is missing a selection digest")
	}
	if limits.BlockFrames <= 0 {
		limits.BlockFrames = defaultBlockFrames
	}
	if limits.BlockFrames < 64 {
		limits.BlockFrames = 64
	}
	engine := &streamEngine{
		ctx:         ctx,
		renderID:    renderID,
		plan:        plan,
		sources:     sources,
		limits:      limits,
		channels:    plan.OutputChannels,
		sampleRate:  plan.SampleRate,
		blockFrames: limits.BlockFrames,
		quantSpec:   quantSpec,
		selection:   request.SelectionDigest,
	}
	if request.Checkpoint != nil {
		checkpoint := request.Checkpoint
		if checkpoint.SelectionDigest != request.SelectionDigest {
			return nil, errors.New("render checkpoint belongs to a different source selection or quantization specification")
		}
		if checkpoint.QuantizerDigest != quantSpec.Digest() {
			return nil, errors.New("render checkpoint quantizer state does not match the selected quantization specification")
		}
		if checkpoint.FrameIndex < 0 || checkpoint.Gain <= 0 || math.IsNaN(checkpoint.Gain) {
			return nil, errors.New("render checkpoint is inconsistent")
		}
		engine.startFrame = checkpoint.FrameIndex
		engine.normPeak = checkpoint.Peak
		engine.normGain = checkpoint.Gain
		engine.outputPeak = checkpoint.OutputPeak
		engine.outputSquared = checkpoint.OutputSquared
		engine.outputSamples = checkpoint.OutputSamples
	}
	quantizer, err := audiocodec.NewQuantizer(quantSpec, request.Seed, engine.startFrame)
	if err != nil {
		return nil, err
	}
	if request.Checkpoint != nil {
		if err := quantizer.LoadState(request.Checkpoint.Quantizer); err != nil {
			return nil, err
		}
	}
	engine.quantizer = quantizer
	engine.measurement = newMeasurementAccumulator(quantSpec, limits.BlockFrames)
	if request.Checkpoint != nil {
		if err := engine.measurement.Load(request.Checkpoint.Measurement); err != nil {
			return nil, err
		}
	}
	if err := engine.prepareSpans(); err != nil {
		engine.close()
		return nil, err
	}
	if engine.startFrame > engine.frames {
		engine.close()
		return nil, errors.New("render checkpoint frame index is outside the timeline")
	}
	return engine, nil
}

func measurementWindowFor(blockFrames int) int {
	size := 1024
	if blockFrames > size {
		size = blockFrames
	}
	return size
}

func (engine *streamEngine) prepareSpans() error {
	known := make(map[string]SourceFile)
	readers := make(map[string]audiocodec.FrameReader)
	cursor := int64(0)
	previousFrames := int64(0)
	for index, planSpan := range engine.plan.Spans {
		file, cached := known[planSpan.SourceID]
		if !cached {
			opened, err := engine.sources.OpenSource(engine.ctx, planSpan.SourceID)
			if err != nil {
				return err
			}
			file = opened
			known[planSpan.SourceID] = file
		}
		reader, cached := readers[planSpan.SourceID]
		if !cached {
			opened, err := engine.sources.OpenReader(engine.ctx, file)
			if err != nil {
				return err
			}
			reader = opened
			readers[planSpan.SourceID] = reader
		}
		info := reader.Info()
		if planSpan.SourceStartFrame < 0 ||
			planSpan.SourceEndFrame <= planSpan.SourceStartFrame ||
			planSpan.SourceEndFrame > info.FrameCount {
			return errors.New("span source interval is outside the source frame count")
		}
		sliceFrames := planSpan.SourceEndFrame - planSpan.SourceStartFrame
		outputFrames := domain.ScaledFrames(sliceFrames, info.SampleRate, engine.sampleRate)
		if outputFrames < 1 {
			return errors.New("span becomes empty at the timeline sample rate")
		}
		start := cursor
		if index > 0 {
			if planSpan.CrossfadeFrames > previousFrames || planSpan.CrossfadeFrames > outputFrames {
				return errors.New("crossfade exceeds a neighboring span")
			}
			start -= planSpan.CrossfadeFrames
		}
		if start < 0 {
			return errors.New("crossfade produced a negative output position")
		}
		remap := remapNone
		if info.Channels == 1 && engine.channels == 2 {
			remap = remapMonoToStereo
		} else if info.Channels == 2 && engine.channels == 1 {
			remap = remapStereoToMono
		}
		engine.spans = append(engine.spans, streamSpan{
			Index:            index,
			SourceID:         planSpan.SourceID,
			SourceDigest:     file.Asset.Digest,
			SourceStartFrame: planSpan.SourceStartFrame,
			SourceEndFrame:   planSpan.SourceEndFrame,
			SourceRate:       info.SampleRate,
			SourceChannels:   info.Channels,
			OutputStartFrame: start,
			OutputEndFrame:   start + outputFrames,
			OutputFrames:     outputFrames,
			CrossfadeFrames:  planSpan.CrossfadeFrames,
			GainDB:           planSpan.GainDB,
			ChannelAction:    channelActionName(info.Channels, engine.channels),
			file:             file,
			reader:           reader,
			gain:             float32(audiocodec.DBToLinear(planSpan.GainDB)),
			remap:            remap,
			step:             float64(info.SampleRate) / float64(engine.sampleRate),
			sliced:           info.SampleRate == engine.sampleRate,
			sliceFrames:      sliceFrames,
		})
		cursor = start + outputFrames
		previousFrames = outputFrames
	}
	if cursor < 1 {
		return errors.New("timeline produced no output frames")
	}
	if engine.limits.MaxOutputFrames > 0 && cursor > engine.limits.MaxOutputFrames {
		return errors.New("rendered timeline exceeds the configured frame limit")
	}
	engine.frames = cursor
	return nil
}

func channelActionName(sourceChannels, outputChannels int) string {
	if sourceChannels == outputChannels {
		return "unchanged"
	}
	if sourceChannels == 1 {
		return "mono_to_stereo"
	}
	return "stereo_to_mono"
}

func (engine *streamEngine) close() {
	for index := range engine.spans {
		if engine.spans[index].reader != nil {
			_ = engine.spans[index].reader.Close()
			engine.spans[index].reader = nil
		}
	}
}

func (engine *streamEngine) rewindReaders() error {
	for index := range engine.spans {
		if engine.spans[index].reader != nil {
			_ = engine.spans[index].reader.Close()
			engine.spans[index].reader = nil
		}
		opened, err := engine.sources.OpenReader(engine.ctx, engine.spans[index].file)
		if err != nil {
			return err
		}
		engine.spans[index].reader = opened
	}
	return nil
}

func (engine *streamEngine) render(path string) (StreamResult, error) {
	if engine.normGain <= 0 {
		if err := engine.rewindReaders(); err != nil {
			return StreamResult{}, err
		}
		peak, err := engine.computePeak()
		if err != nil {
			return StreamResult{}, err
		}
		engine.normPeak = peak
		engine.normGain = normalizationGain(peak, engine.plan.TargetPeakDBFS)
	}
	if err := engine.rewindReaders(); err != nil {
		return StreamResult{}, err
	}
	resume := engine.startFrame > 0
	writer, err := newOutputWriter(path, engine.sampleRate, engine.channels, engine.frames, engine.startFrame, resume)
	if err != nil {
		return StreamResult{}, err
	}
	if err := engine.writePass(writer); err != nil {
		_ = writer.close()
		return StreamResult{}, err
	}
	if err := writer.sync(); err != nil {
		_ = writer.close()
		return StreamResult{}, err
	}
	digest, byteCount, err := writer.digest()
	if err != nil {
		_ = writer.close()
		return StreamResult{}, err
	}
	if err := writer.close(); err != nil {
		return StreamResult{}, err
	}
	peakDBFS, rmsDBFS := engine.outputMetrics()
	output := domain.OutputAsset{
		Digest:        digest,
		ByteCount:     byteCount,
		SampleRate:    engine.sampleRate,
		Channels:      engine.channels,
		BitsPerSample: 16,
		FrameCount:    engine.frames,
		DurationMS:    durationMilliseconds(engine.frames, engine.sampleRate),
		PeakDBFS:      peakDBFS,
		RMSDBFS:       rmsDBFS,
	}
	receipt := buildStreamReceipt(engine, output, time.Now().UTC())
	finalState := RenderCheckpoint{
		SelectionDigest: engine.selection,
		QuantizerDigest: engine.quantSpec.Digest(),
		FrameIndex:      engine.frames,
		StartFrame:      0,
		Peak:            engine.normPeak,
		Gain:            engine.normGain,
		Quantizer:       engine.quantizer.State(),
		Measurement:     engine.measurement.State(),
		OutputPeak:      engine.outputPeak,
		OutputSquared:   engine.outputSquared,
		OutputSamples:   engine.outputSamples,
	}
	return StreamResult{OutputPath: path, Output: output, Receipt: receipt, FinalState: finalState}, nil
}

func normalizationGain(peak, targetDBFS float64) float64 {
	target := audiocodec.DBToLinear(targetDBFS)
	gain := 1.0
	if peak > 0 {
		gain = target / peak
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		return 1
	}
	if peak*gain > 1 {
		gain = 1 / peak
	}
	return gain
}

func (engine *streamEngine) limitBlock(start int64) int64 {
	count := int64(engine.blockFrames)
	if start+count > engine.frames {
		count = engine.frames - start
	}
	return count
}

func (engine *streamEngine) computePeak() (float64, error) {
	block := make([]float32, engine.blockFrames*engine.channels)
	peak := 0.0
	for start := int64(0); start < engine.frames; {
		count := engine.limitBlock(start)
		sampleCount := count * int64(engine.channels)
		if err := engine.mixBlock(start, count, block[:sampleCount]); err != nil {
			return 0, err
		}
		for _, sample := range block[:sampleCount] {
			absolute := math.Abs(float64(sample))
			if absolute > peak {
				peak = absolute
			}
		}
		start += count
		if err := engine.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return peak, nil
}

func (engine *streamEngine) writePass(writer *outputWriter) error {
	block := make([]float32, engine.blockFrames*engine.channels)
	quantized := make([]int16, engine.blockFrames*engine.channels)
	nextCheckpoint := engine.startFrame + checkpointIntervalFrames
	for start := engine.startFrame; start < engine.frames; {
		count := engine.limitBlock(start)
		sampleCount := count * int64(engine.channels)
		mix := block[:sampleCount]
		if err := engine.mixBlock(start, count, mix); err != nil {
			return err
		}
		gain := engine.normGain
		if gain != 1 {
			for index := range mix {
				value := float64(mix[index]) * gain
				if value > 1 {
					value = 1
				} else if value < -1 {
					value = -1
				}
				mix[index] = float32(value)
			}
		}
		if err := engine.quantizer.QuantizeBlock(mix, quantized[:sampleCount]); err != nil {
			return err
		}
		if err := writer.writeBlock(quantized[:sampleCount]); err != nil {
			return err
		}
		engine.measurement.Observe(mix, quantized[:sampleCount], engine.quantSpec)
		for _, sample := range quantized[:sampleCount] {
			value := float64(audiocodec.DecodePCM16Sample(sample))
			absolute := math.Abs(value)
			if absolute > engine.outputPeak {
				engine.outputPeak = absolute
			}
			engine.outputSquared += value * value
			engine.outputSamples++
		}
		start += count
		if err := engine.ctx.Err(); err != nil {
			if persistErr := engine.persistCheckpoint(writer, start); persistErr != nil {
				return persistErr
			}
			return err
		}
		if engine.checkpointSink != nil && start >= nextCheckpoint && start < engine.frames {
			if err := engine.persistCheckpoint(writer, start); err != nil {
				return err
			}
			nextCheckpoint = start + checkpointIntervalFrames
		}
	}
	if engine.checkpointSink != nil {
		if err := engine.persistCheckpoint(writer, engine.frames); err != nil {
			return err
		}
	}
	return nil
}

func (engine *streamEngine) outputMetrics() (float64, float64) {
	peak := 0.0
	if engine.outputSamples > 0 && engine.outputSquared > 0 {
		peak = engine.outputSquared / float64(engine.outputSamples)
	}
	peakDBFS := audiocodec.LinearToDB(engine.outputPeak)
	rmsDBFS := audiocodec.LinearToDB(math.Sqrt(peak))
	return peakDBFS, rmsDBFS
}

func (engine *streamEngine) persistCheckpoint(writer *outputWriter, frameIndex int64) error {
	if engine.checkpointSink == nil {
		return nil
	}
	if err := writer.sync(); err != nil {
		return err
	}
	checkpoint := RenderCheckpoint{
		SelectionDigest: engine.selection,
		QuantizerDigest: engine.quantSpec.Digest(),
		FrameIndex:      frameIndex,
		StartFrame:      engine.startFrame,
		Peak:            engine.normPeak,
		Gain:            engine.normGain,
		Quantizer:       engine.quantizer.State(),
		Measurement:     engine.measurement.State(),
		OutputPeak:      engine.outputPeak,
		OutputSquared:   engine.outputSquared,
		OutputSamples:   engine.outputSamples,
	}
	return engine.checkpointSink(checkpoint)
}

func (engine *streamEngine) mixBlock(start, count int64, block []float32) error {
	for index := range block {
		block[index] = 0
	}
	for spanIndex := range engine.spans {
		if err := engine.mixSpan(&engine.spans[spanIndex], start, count, block); err != nil {
			return err
		}
	}
	return nil
}

func (engine *streamEngine) mixSpan(span *streamSpan, start, count int64, block []float32) error {
	blockEnd := start + count
	if blockEnd <= span.OutputStartFrame || start >= span.OutputEndFrame {
		return nil
	}
	localStart := start
	if localStart < span.OutputStartFrame {
		localStart = span.OutputStartFrame
	}
	localEnd := blockEnd
	if localEnd > span.OutputEndFrame {
		localEnd = span.OutputEndFrame
	}
	firstSpanFrame := localStart - span.OutputStartFrame
	lastSpanFrame := localEnd - span.OutputStartFrame
	readStart, readEnd, err := sourceRange(span, firstSpanFrame, lastSpanFrame)
	if err != nil {
		return err
	}
	needed := (readEnd - readStart) * int64(span.SourceChannels)
	if int64(cap(engine.scratch)) < needed {
		engine.scratch = make([]float32, needed)
	}
	sourceSamples := engine.scratch[:needed]
	if err := span.reader.ReadFrames(engine.ctx, readStart, readEnd-readStart, sourceSamples); err != nil {
		return err
	}
	for outputFrame := localStart; outputFrame < localEnd; outputFrame++ {
		spanFrame := outputFrame - span.OutputStartFrame
		incoming, outgoing := fadeWeights(span, spanFrame)
		blockOffset := (outputFrame - start) * int64(engine.channels)
		for channel := 0; channel < engine.channels; channel++ {
			value, err := spanSample(span, channel, spanFrame, readStart, sourceSamples)
			if err != nil {
				return err
			}
			value *= span.gain * incoming
			if span.CrossfadeFrames > 0 && spanFrame < span.CrossfadeFrames {
				block[blockOffset+int64(channel)] *= outgoing
			}
			block[blockOffset+int64(channel)] += value
		}
	}
	return nil
}

func sourceRange(span *streamSpan, firstFrame, endFrame int64) (int64, int64, error) {
	if span.sliced {
		return span.SourceStartFrame + firstFrame, span.SourceStartFrame + endFrame, nil
	}
	firstPosition := float64(firstFrame) * span.step
	lastPosition := float64(endFrame-1) * span.step
	left := int64(math.Floor(firstPosition))
	right := int64(math.Floor(lastPosition)) + 1
	if left < 0 {
		left = 0
	}
	if right > span.sliceFrames-1 {
		right = span.sliceFrames - 1
	}
	if right < left {
		right = left
	}
	return span.SourceStartFrame + left, span.SourceStartFrame + right + 1, nil
}

func spanSample(span *streamSpan, channel int, spanFrame, readStart int64, source []float32) (float32, error) {
	if span.sliced {
		index := (span.SourceStartFrame + spanFrame - readStart) * int64(span.SourceChannels)
		if index < 0 || index+int64(span.SourceChannels) > int64(len(source)) {
			return 0, errors.New("span sample is outside the rolling window")
		}
		switch span.remap {
		case remapStereoToMono:
			return (source[index] + source[index+1]) * 0.5, nil
		case remapMonoToStereo:
			return source[index], nil
		default:
			return source[index+int64(channel)], nil
		}
	}
	position := float64(spanFrame) * span.step
	left := int64(position)
	if left >= span.sliceFrames-1 {
		left = span.sliceFrames - 1
	}
	if left < 0 {
		left = 0
	}
	right := left + 1
	if right >= span.sliceFrames {
		right = span.sliceFrames - 1
	}
	weight := float32(position - float64(left))
	leftIndex := (span.SourceStartFrame + left - readStart) * int64(span.SourceChannels)
	rightIndex := (span.SourceStartFrame + right - readStart) * int64(span.SourceChannels)
	if leftIndex < 0 || rightIndex+int64(span.SourceChannels) > int64(len(source)) {
		return 0, errors.New("span resampling window is outside the source")
	}
	var first, second float32
	switch span.remap {
	case remapStereoToMono:
		first = (source[leftIndex] + source[leftIndex+1]) * 0.5
		second = (source[rightIndex] + source[rightIndex+1]) * 0.5
	case remapMonoToStereo:
		first = source[leftIndex]
		second = source[rightIndex]
	default:
		first = source[leftIndex+int64(channel)]
		second = source[rightIndex+int64(channel)]
	}
	return first + (second-first)*weight, nil
}

func fadeWeights(span *streamSpan, spanFrame int64) (float32, float32) {
	if span.CrossfadeFrames <= 0 || spanFrame >= span.CrossfadeFrames {
		return 1, 1
	}
	progress := (float64(spanFrame) + 0.5) / float64(span.CrossfadeFrames)
	angle := progress * math.Pi / 2
	return float32(math.Sin(angle)), float32(math.Cos(angle))
}

func durationMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}

func buildStreamReceipt(engine *streamEngine, output domain.OutputAsset, now time.Time) domain.RenderReceipt {
	receiptSpans := make([]domain.ReceiptSpan, 0, len(engine.spans))
	warnings := make([]string, 0)
	for _, span := range engine.spans {
		receiptSpans = append(receiptSpans, domain.ReceiptSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		})
		if span.ChannelAction != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.Index, span.ChannelAction))
		}
	}
	profile := engine.quantSpec.ProfileReport()
	quantization := domain.QuantizationReceipt{
		Mode:                  string(engine.quantSpec.Mode),
		SpecDigest:            engine.quantSpec.Digest(),
		ShaperProfile:         engine.quantSpec.ShaperProfile,
		ShaperTaps:            maxInt(0, len(engine.quantSpec.ShaperTaps)-1),
		DitherLSB:             engine.quantSpec.DitherLSBP2P,
		AudibleBandLowHz:      profile.AudibleBandLowHz,
		AudibleBandHighHz:     profile.AudibleBandHighHz,
		AudibleBandGainDB:     profile.AudibleBandGainDB,
		OutOfBandGainDB:       profile.OutOfBandGainDB,
		MeasurementFrames:     engine.measurement.Frames(),
		MeasuredBandFloorDBFS: engine.measurement.AudibleFloorDBFS(),
		MeasuredOutBandDBFS:   engine.measurement.OutOfBandFloorDBFS(),
		LegacyBandFloorDBFS:   engine.measurement.LegacyAudibleFloorDBFS(),
		AudibleFloorDeltaDB:   engine.measurement.AudibleFloorDeltaDB(),
		SelectionDigest:       engine.selection,
		RestartStrategy:       "deterministic_checkpoint",
	}
	if engine.startFrame > 0 {
		quantization.RestartStrategy = "deterministic_checkpoint_resume"
	}
	return domain.RenderReceipt{
		SchemaVersion: 2,
		RenderID:      engine.renderID,
		TimelineID:    engine.plan.ID,
		Output:        output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: engine.plan.TargetPeakDBFS,
			BeforePeakDBFS:    audiocodec.LinearToDB(engine.normPeak),
			AppliedGainDB:     audiocodec.LinearToDB(engine.normGain),
			OutputPeakDBFS:    output.PeakDBFS,
		},
		Quantization: quantization,
		Spans:        receiptSpans,
		Warnings:     warnings,
		CreatedAt:    now.UTC(),
	}
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// newOutputWriter creates or resumes the PCM16 WAV. Resume seeks to the
// checkpoint frame and continues writing; earlier bytes are left untouched.
func newOutputWriter(path string, sampleRate, channels int, frameCount, startFrame int64, resume bool) (*outputWriter, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if resume {
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		offset := int64(44) + startFrame*int64(channels)*2
		if info.Size() < offset {
			_ = file.Close()
			return nil, errors.New("partial render is shorter than its checkpoint")
		}
		existingHeader := make([]byte, 44)
		if _, err := file.ReadAt(existingHeader, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
		expected, err := wavHeader(sampleRate, channels, frameCount)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if !bytes.Equal(existingHeader, expected) {
			_ = file.Close()
			return nil, errors.New("partial render header does not match this selection")
		}
		if _, err := file.Seek(offset, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
	} else {
		header, err := wavHeader(sampleRate, channels, frameCount)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		// A fresh render always replaces stale bytes from any previous partial
		// attempt, so the published file exactly matches this run.
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
		if _, err := file.WriteAt(header, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
		if _, err := file.Seek(44, 0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return &outputWriter{file: file, channels: channels, sampleRate: sampleRate}, nil
}

type outputWriter struct {
	file       *os.File
	channels   int
	sampleRate int
	payload    []byte
}

func (writer *outputWriter) writeBlock(quantized []int16) error {
	needed := len(quantized) * 2
	if cap(writer.payload) < needed {
		writer.payload = make([]byte, needed)
	}
	payload := writer.payload[:needed]
	for index, value := range quantized {
		putUint16(payload[index*2:index*2+2], uint16(value))
	}
	if _, err := writer.file.Write(payload); err != nil {
		return err
	}
	return nil
}

func (writer *outputWriter) sync() error { return writer.file.Sync() }

func (writer *outputWriter) close() error { return writer.file.Close() }

func (writer *outputWriter) digest() (string, int64, error) {
	if _, err := writer.file.Seek(0, 0); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	buffer := make([]byte, 1<<20)
	var total int64
	for {
		read, err := writer.file.Read(buffer)
		if read > 0 {
			hasher.Write(buffer[:read])
			total += int64(read)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", 0, err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}

func wavHeader(sampleRate, channels int, frameCount int64) ([]byte, error) {
	dataBytes := uint64(frameCount) * uint64(channels) * 2
	if dataBytes > math.MaxUint32-36 {
		return nil, errors.New("encoded PCM payload is too large")
	}
	header := make([]byte, 44)
	byteCount := uint64(44) + dataBytes
	copy(header[0:4], "RIFF")
	putUint32(header[4:8], uint32(byteCount-8))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	putUint32(header[16:20], 16)
	putUint16(header[20:22], 1)
	putUint16(header[22:24], uint16(channels))
	putUint32(header[24:28], uint32(sampleRate))
	putUint32(header[28:32], uint32(sampleRate*channels*2))
	putUint16(header[32:34], uint16(channels*2))
	putUint16(header[34:36], 16)
	copy(header[36:40], "data")
	putUint32(header[40:44], uint32(dataBytes))
	return header, nil
}

func putUint32(target []byte, value uint32) {
	target[0] = byte(value)
	target[1] = byte(value >> 8)
	target[2] = byte(value >> 16)
	target[3] = byte(value >> 24)
}

func putUint16(target []byte, value uint16) {
	target[0] = byte(value)
	target[1] = byte(value >> 8)
}
