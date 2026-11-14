package audiocodec

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// QuantizationMode selects how normalized float samples become 16-bit PCM.
// Legacy is the historical round-to-nearest encoding and stays byte for byte
// compatible. NoiseShaped keeps the same 16-bit container but moves
// quantization error out of the audible band with deterministic error
// feedback plus shaped TPDF dither.
type QuantizationMode string

const (
	QuantizationLegacy      QuantizationMode = "legacy"
	QuantizationNoiseShaped QuantizationMode = "noise_shaped_v1"
)

const (
	// shaperOrder is the number of error-feedback taps.
	shaperOrder = 33
	// shaperRegularization balances audible-band suppression against the
	// out-of-band amplification that pays for it.
	shaperRegularization = 0.02
	// audibleBandLowHz is the lower edge of the published audible band.
	audibleBandLowHz = 20.0
	// audibleBandHighCapHz caps the audible band for high sample rates.
	audibleBandHighCapHz = 16000.0
	// audibleBandEdgeRatio keeps the band inside Nyquist for low rates.
	audibleBandEdgeRatio = 0.85
	// ditherLSBP2P is the peak-to-peak TPDF dither in 16-bit LSBs. Half an
	// LSB of TPDF removes idle tones without raising the audited floor much.
	ditherLSBP2P = 0.5
	// feedbackClampLSB bounds instantaneous feedback so a step between quiet
	// and full-scale passages cannot amplify stale residual state.
	feedbackClampLSB = 4.0
	// residualClampLSB bounds the error history retained per decision.
	residualClampLSB = 1.0
)

// QuantizationSpec is the public description of one 16-bit encoding policy.
type QuantizationSpec struct {
	Mode          QuantizationMode `json:"mode"`
	SampleRate    int              `json:"sample_rate"`
	Channels      int              `json:"channels"`
	BitsPerSample int              `json:"bits_per_sample"`
	DitherLSBP2P  float64          `json:"dither_lsb"`
	ShaperProfile string           `json:"shaper_profile"`
	ShaperTaps    []float64        `json:"shaper_taps"`
}

func ParseQuantizationMode(value string) (QuantizationMode, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", string(QuantizationLegacy):
		return QuantizationLegacy, nil
	case string(QuantizationNoiseShaped):
		return QuantizationNoiseShaped, nil
	default:
		return "", errors.New("quantization mode must be legacy or noise_shaped_v1")
	}
}

func BuildQuantizationSpec(mode QuantizationMode, sampleRate, channels int) (QuantizationSpec, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return QuantizationSpec{}, errors.New("sample rate is outside the accepted range")
	}
	if channels != 1 && channels != 2 {
		return QuantizationSpec{}, errors.New("channel count must be one or two")
	}
	switch mode {
	case QuantizationLegacy:
		return QuantizationSpec{
			Mode:          QuantizationLegacy,
			SampleRate:    sampleRate,
			Channels:      channels,
			BitsPerSample: pcmBitWidth,
			ShaperProfile: "none",
		}, nil
	case QuantizationNoiseShaped:
		taps, err := DesignShapingTaps(sampleRate)
		if err != nil {
			return QuantizationSpec{}, err
		}
		return QuantizationSpec{
			Mode:          QuantizationNoiseShaped,
			SampleRate:    sampleRate,
			Channels:      channels,
			BitsPerSample: pcmBitWidth,
			DitherLSBP2P:  ditherLSBP2P,
			ShaperProfile: "audible_band_ls_v1",
			ShaperTaps:    taps,
		}, nil
	default:
		return QuantizationSpec{}, errors.New("unsupported quantization mode")
	}
}

// AudibleBandLowHz returns the lower edge of the published audible band.
func AudibleBandLowHz() float64 { return audibleBandLowHz }

// AudibleBandEdgeHz returns the upper edge of the band the shaper protects.
func AudibleBandEdgeHz(sampleRate int) float64 {
	edge := audibleBandHighCapHz
	if nyquist := float64(sampleRate) / 2; nyquist*audibleBandEdgeRatio < edge {
		edge = nyquist * audibleBandEdgeRatio
	}
	return edge
}

// ShapingProfileReport is a deterministic, auditable description of the
// transfer function. Gains are average power ratios in dB, so the audible-band
// number states exactly how much error power moved out of the band.
type ShapingProfileReport struct {
	AudibleBandLowHz  float64 `json:"audible_band_low_hz"`
	AudibleBandHighHz float64 `json:"audible_band_high_hz"`
	AudibleBandGainDB float64 `json:"audible_band_gain_db"`
	OutOfBandGainDB   float64 `json:"out_of_band_gain_db"`
	TotalGainDB       float64 `json:"total_gain_db"`
}

func (spec QuantizationSpec) ProfileReport() ShapingProfileReport {
	report := ShapingProfileReport{
		AudibleBandLowHz:  audibleBandLowHz,
		AudibleBandHighHz: AudibleBandEdgeHz(spec.SampleRate),
	}
	if len(spec.ShaperTaps) == 0 {
		return report
	}
	report.AudibleBandGainDB = bandGainDB(spec.ShaperTaps, report.AudibleBandLowHz, report.AudibleBandHighHz, spec.SampleRate)
	report.OutOfBandGainDB = bandGainDB(spec.ShaperTaps, report.AudibleBandHighHz, float64(spec.SampleRate)/2, spec.SampleRate)
	report.TotalGainDB = 10 * math.Log10(sumSquares(spec.ShaperTaps)+1e-30)
	return report
}

func bandGainDB(taps []float64, low, high float64, sampleRate int) float64 {
	if high <= low {
		return 0
	}
	const steps = 2048
	total := 0.0
	for step := 0; step < steps; step++ {
		frequency := low + (high-low)*(float64(step)+0.5)/float64(steps)
		angle := 2 * math.Pi * frequency / float64(sampleRate)
		real := 0.0
		imaginary := 0.0
		for order, tap := range taps {
			real += tap * math.Cos(float64(order)*angle)
			imaginary -= tap * math.Sin(float64(order)*angle)
		}
		total += real*real + imaginary*imaginary
	}
	return 10 * math.Log10(total/float64(steps)+1e-30)
}

func sumSquares(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value * value
	}
	return total
}

func (spec QuantizationSpec) Digest() string {
	parts := []string{
		string(spec.Mode),
		strconv.Itoa(spec.SampleRate),
		strconv.Itoa(spec.Channels),
		strconv.Itoa(spec.BitsPerSample),
		strconv.FormatFloat(spec.DitherLSBP2P, 'g', 17, 64),
		spec.ShaperProfile,
	}
	for _, tap := range spec.ShaperTaps {
		parts = append(parts, strconv.FormatFloat(tap, 'g', 17, 64))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])
}

func (spec QuantizationSpec) Describe() string {
	if spec.Mode == QuantizationNoiseShaped {
		report := spec.ProfileReport()
		return fmt.Sprintf(
			"16-bit PCM %s: %d-tap shaper, audible band %.0f-%.0f Hz gain %+.2f dB, out-of-band %+.2f dB, %.2f LSB TPDF",
			spec.ShaperProfile, len(spec.ShaperTaps)-1, report.AudibleBandLowHz, report.AudibleBandHighHz,
			report.AudibleBandGainDB, report.OutOfBandGainDB, spec.DitherLSBP2P,
		)
	}
	return "16-bit PCM legacy round-to-nearest without dither or error feedback"
}

// DesignShapingTaps returns the error-feedback taps C[z] with C[0]=1. The loop
// computes q = Q(x + sum_{k>=1} C[k] e[n-k]), so the delivered error spectrum
// is |C(e^jw)| times the quantizer error spectrum. Taps are solved from a
// least-squares objective over the audible band with a quadratic penalty on
// total error power. The solve depends only on the sample rate.
func DesignShapingTaps(sampleRate int) ([]float64, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, errors.New("sample rate is outside the accepted range")
	}
	const order = shaperOrder
	w1 := 2 * math.Pi * audibleBandLowHz / float64(sampleRate)
	w2 := 2 * math.Pi * AudibleBandEdgeHz(sampleRate) / float64(sampleRate)
	kernel := make([]float64, order)
	for d := 0; d < order; d++ {
		if d == 0 {
			kernel[d] = (w2 - w1) / math.Pi
		} else {
			kernel[d] = (math.Sin(float64(d)*w2) - math.Sin(float64(d)*w1)) / (float64(d) * math.Pi)
		}
	}
	sub := make([][]float64, order-1)
	for i := 0; i < order-1; i++ {
		sub[i] = make([]float64, order-1)
		for j := 0; j < order-1; j++ {
			sub[i][j] = kernel[absInt(i-j)]
		}
		sub[i][i] += shaperRegularization
	}
	rhs := make([]float64, order-1)
	for i := 1; i < order; i++ {
		rhs[i-1] = -kernel[i]
	}
	solution, err := solveLinearSystem(sub, rhs)
	if err != nil {
		return nil, fmt.Errorf("cannot design noise shaper: %w", err)
	}
	taps := make([]float64, order)
	taps[0] = 1
	copy(taps[1:], solution)
	feedbackL1 := 0.0
	maxTap := 0.0
	for _, tap := range taps[1:] {
		if math.IsNaN(tap) || math.IsInf(tap, 0) {
			return nil, errors.New("noise shaper design is not finite at this sample rate")
		}
		feedbackL1 += math.Abs(tap)
		if math.Abs(tap) > maxTap {
			maxTap = math.Abs(tap)
		}
	}
	if maxTap > 1.2 || feedbackL1 > feedbackClampLSB*2 {
		return nil, errors.New("noise shaper design is not numerically stable at this sample rate")
	}
	return taps, nil
}

func solveLinearSystem(matrix [][]float64, rhs []float64) ([]float64, error) {
	n := len(rhs)
	if n == 0 || len(matrix) != n {
		return nil, errors.New("linear system dimensions are invalid")
	}
	work := make([][]float64, n)
	for i := 0; i < n; i++ {
		if len(matrix[i]) != n {
			return nil, errors.New("linear system matrix is not square")
		}
		work[i] = make([]float64, n+1)
		copy(work[i], matrix[i])
		work[i][n] = rhs[i]
	}
	for column := 0; column < n; column++ {
		pivot := column
		for row := column + 1; row < n; row++ {
			if math.Abs(work[row][column]) > math.Abs(work[pivot][column]) {
				pivot = row
			}
		}
		if math.Abs(work[pivot][column]) < 1e-14 {
			return nil, errors.New("linear system is singular")
		}
		if pivot != column {
			work[column], work[pivot] = work[pivot], work[column]
		}
		pivotValue := work[column][column]
		pivotRow := work[column]
		for row := column + 1; row < n; row++ {
			scale := work[row][column] / pivotValue
			if scale == 0 {
				continue
			}
			target := work[row]
			for c := column; c <= n; c++ {
				target[c] -= scale * pivotRow[c]
			}
		}
	}
	solution := make([]float64, n)
	for row := n - 1; row >= 0; row-- {
		sum := work[row][n]
		for c := row + 1; c < n; c++ {
			sum -= work[row][c] * solution[c]
		}
		solution[row] = sum / work[row][row]
	}
	return solution, nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// LegacyQuantize reproduces the historical 16-bit rounding exactly.
func LegacyQuantize(sample float32) int16 {
	var quantized int32
	switch {
	case sample <= -1:
		quantized = -32768
	case sample >= 1:
		quantized = 32767
	case sample < 0:
		quantized = int32(math.Round(float64(sample) * 32768))
	default:
		quantized = int32(math.Round(float64(sample) * 32767))
	}
	if quantized < -32768 {
		quantized = -32768
	}
	if quantized > 32767 {
		quantized = 32767
	}
	return int16(quantized)
}

// DecodePCM16Sample maps stored PCM back to normalized float exactly as
// DecodeWAV does.
func DecodePCM16Sample(raw int16) float32 {
	if raw < 0 {
		return float32(raw) / 32768
	}
	return float32(raw) / 32767
}

// QuantizerState is the durable continuation of a quantizer. It is persisted
// beside a render checkpoint so a resumed run reproduces the same PCM.
type QuantizerState struct {
	SpecDigest string      `json:"spec_digest"`
	FrameIndex int64       `json:"frame_index"`
	History    [][]float64 `json:"history"`
}

type quantizerChannel struct {
	history []float64
}

type Quantizer struct {
	spec       QuantizationSpec
	channels   []quantizerChannel
	frameIndex int64
	seed       uint64
}

func NewQuantizer(spec QuantizationSpec, seed uint64, startFrame int64) (*Quantizer, error) {
	if spec.Channels != 1 && spec.Channels != 2 {
		return nil, errors.New("quantizer channel count is invalid")
	}
	if spec.Mode != QuantizationLegacy && spec.Mode != QuantizationNoiseShaped {
		return nil, errors.New("unsupported quantization mode")
	}
	if spec.Mode == QuantizationNoiseShaped && len(spec.ShaperTaps) == 0 {
		return nil, errors.New("noise-shaped quantization requires shaper taps")
	}
	if startFrame < 0 {
		return nil, errors.New("quantizer start frame is negative")
	}
	historySize := 0
	if spec.Mode == QuantizationNoiseShaped {
		historySize = len(spec.ShaperTaps) - 1
	}
	quantizer := &Quantizer{
		spec:       spec,
		channels:   make([]quantizerChannel, spec.Channels),
		frameIndex: startFrame,
		seed:       seed,
	}
	for channel := range quantizer.channels {
		quantizer.channels[channel].history = make([]float64, historySize)
	}
	return quantizer, nil
}

func (quantizer *Quantizer) Spec() QuantizationSpec { return quantizer.spec }

func (quantizer *Quantizer) FrameIndex() int64 { return quantizer.frameIndex }

func (quantizer *Quantizer) State() QuantizerState {
	history := make([][]float64, len(quantizer.channels))
	for index := range quantizer.channels {
		history[index] = append([]float64(nil), quantizer.channels[index].history...)
	}
	return QuantizerState{SpecDigest: quantizer.spec.Digest(), FrameIndex: quantizer.frameIndex, History: history}
}

func (quantizer *Quantizer) LoadState(state QuantizerState) error {
	if state.SpecDigest != quantizer.spec.Digest() {
		return errors.New("quantizer state belongs to a different quantization specification")
	}
	if len(state.History) != len(quantizer.channels) {
		return errors.New("quantizer state channel count does not match")
	}
	if state.FrameIndex < 0 {
		return errors.New("quantizer state frame index is negative")
	}
	for channel := range quantizer.channels {
		if len(state.History[channel]) != len(quantizer.channels[channel].history) {
			return errors.New("quantizer state history length does not match")
		}
		quantizer.channels[channel].history = append([]float64(nil), state.History[channel]...)
	}
	quantizer.frameIndex = state.FrameIndex
	return nil
}

// QuantizeBlock converts one interleaved, normalized float block to PCM16.
func (quantizer *Quantizer) QuantizeBlock(samples []float32, out []int16) error {
	channels := quantizer.spec.Channels
	if len(samples)%channels != 0 {
		return errors.New("quantizer block is not frame aligned")
	}
	if len(out) != len(samples) {
		return errors.New("quantizer output buffer does not match the block size")
	}
	if quantizer.spec.Mode == QuantizationLegacy {
		for index, sample := range samples {
			out[index] = LegacyQuantize(sample)
		}
		quantizer.frameIndex += int64(len(samples) / channels)
		return nil
	}
	frames := len(samples) / channels
	for frame := 0; frame < frames; frame++ {
		baseIndex := frame * channels
		for channel := 0; channel < channels; channel++ {
			out[baseIndex+channel] = quantizer.quantizeShaped(samples[baseIndex+channel], channel)
		}
		quantizer.frameIndex++
	}
	return nil
}

// quantizeShaped implements error feedback on the full quantization error:
//
//	base = x + sum_{k>=1} C[k] * eps[n-k]
//	probe = base + dither
//	y = Q(probe), eps = y - base
//
// The delivered output error is then exactly
//
//	e[n] = y[n] - x[n] = C(z) * eps[n]
//
// so both the rounding error and the TPDF dither are shaped by C, and the
// in-band error power falls by |C|^2 in the audible band. eps is bounded by
// one LSB (half an LSB of rounding plus half an LSB of dither), so a step
// between quiet and loud passages cannot accumulate unbounded state.
func (quantizer *Quantizer) quantizeShaped(sample float32, channel int) int16 {
	history := quantizer.channels[channel].history
	accumulated := 0.0
	for order := 1; order < len(quantizer.spec.ShaperTaps) && order <= len(history); order++ {
		accumulated += quantizer.spec.ShaperTaps[order] * history[order-1]
	}
	limit := feedbackClampLSB / 32767
	if accumulated > limit {
		accumulated = limit
	} else if accumulated < -limit {
		accumulated = -limit
	}
	base := float64(sample) + accumulated
	probe := base + quantizer.dither(channel)
	saturated := probe >= 1 || probe <= -1
	var quantized int16
	switch {
	case probe <= -1:
		quantized = -32768
	case probe >= 1:
		quantized = 32767
	case probe < 0:
		quantized = int16(math.Round(probe * 32768))
	default:
		quantized = int16(math.Round(probe * 32767))
	}
	residual := float64(DecodePCM16Sample(quantized)) - base
	if saturated {
		// A rail excursion must not leave residual state for the next quiet
		// passage to amplify.
		residual = 0
	} else {
		clamp := residualClampLSB / 32767
		if residual > clamp {
			residual = clamp
		} else if residual < -clamp {
			residual = -clamp
		}
	}
	if len(history) > 0 {
		copy(history[1:], history[:len(history)-1])
		history[0] = residual
	}
	return quantized
}

func (quantizer *Quantizer) dither(channel int) float64 {
	if quantizer.spec.DitherLSBP2P <= 0 {
		return 0
	}
	stream := quantizer.seed ^ uint64(channel+1)*0x9E3779B97F4A7C15
	mixed := stream ^ uint64(quantizer.frameIndex)*0xD1B54A32D192ED03
	first := splitMixUnit(mixed)
	second := splitMixUnit(mixed ^ 0xA24BAED4963EE407)
	// first and second are uniform [0,1); their difference is triangular on
	// [-1,1], so halving converts the 2-LSB span to the reported p2p amplitude.
	return (first - second) * (quantizer.spec.DitherLSBP2P / (2 * 32767))
}

func splitMixUnit(value uint64) float64 {
	value += 0x9E3779B97F4A7C15
	value = (value ^ (value >> 30)) * 0xBF58476D1CE4E5B9
	value = (value ^ (value >> 27)) * 0x94D049BB133111EB
	value = value ^ (value >> 31)
	return float64(value>>11) / float64(uint64(1)<<53)
}

// QuantizerSeed derives a deterministic dither seed from a render identity.
func QuantizerSeed(identity string) uint64 {
	sum := sha256.Sum256([]byte("voice-timeline-quantizer|" + identity))
	var seed uint64
	for index := 0; index < 8; index++ {
		seed = seed<<8 | uint64(sum[index])
	}
	return seed
}
