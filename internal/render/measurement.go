package render

import (
	"errors"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// MeasurementState is the durable accumulation describing the error
// distribution of a render. It stores two averaged power spectra: the shaped
// error actually delivered and, for the noise-shaped mode, the legacy error
// that the same normalized floats would have produced. It never buffers audio.
type MeasurementState struct {
	SpecDigest   string    `json:"spec_digest"`
	SampleRate   int       `json:"sample_rate"`
	Channels     int       `json:"channels"`
	WindowFrames int       `json:"window_frames"`
	Windows      int64     `json:"windows"`
	Frames       int64     `json:"frames"`
	ShapedBins   []float64 `json:"shaped_bins"`
	LegacyBins   []float64 `json:"legacy_bins"`
}

type measurementAccumulator struct {
	state    MeasurementState
	shaped   []float64
	legacy   []float64
	window   []float64
	legacyW  []float64
	spectrum []float64
	imag     []float64
}

func measurementWindowFrames(blockFrames int) int {
	// One block is the analysis window, rounded up to a power of two. This
	// keeps the analysis periodic with checkpoints and allocation bounded.
	size := 1024
	for size < blockFrames {
		size <<= 1
	}
	return size
}

func newMeasurementAccumulator(spec audiocodec.QuantizationSpec, blockFrames int) *measurementAccumulator {
	size := measurementWindowFrames(blockFrames)
	bins := size/2 + 1
	accumulator := &measurementAccumulator{
		state: MeasurementState{
			SpecDigest:   spec.Digest(),
			SampleRate:   spec.SampleRate,
			Channels:     spec.Channels,
			WindowFrames: size,
			ShapedBins:   make([]float64, bins),
			LegacyBins:   make([]float64, bins),
		},
		window:  make([]float64, size),
		legacyW: make([]float64, size),
		imag:    make([]float64, size),
	}
	return accumulator
}

func (accumulator *measurementAccumulator) Frames() int64 { return accumulator.state.Frames }

func (accumulator *measurementAccumulator) State() MeasurementState {
	state := accumulator.state
	state.ShapedBins = append([]float64(nil), accumulator.state.ShapedBins...)
	state.LegacyBins = append([]float64(nil), accumulator.state.LegacyBins...)
	return state
}

func (accumulator *measurementAccumulator) Load(state MeasurementState) error {
	if state.SpecDigest != accumulator.state.SpecDigest {
		return errors.New("measurement state belongs to a different quantization specification")
	}
	if state.WindowFrames != accumulator.state.WindowFrames ||
		state.SampleRate != accumulator.state.SampleRate ||
		state.Channels != accumulator.state.Channels {
		return errors.New("measurement state audio layout does not match")
	}
	if len(state.ShapedBins) != len(accumulator.state.ShapedBins) ||
		len(state.LegacyBins) != len(accumulator.state.LegacyBins) {
		return errors.New("measurement state spectrum dimensions do not match")
	}
	accumulator.state = state
	accumulator.state.ShapedBins = append([]float64(nil), state.ShapedBins...)
	accumulator.state.LegacyBins = append([]float64(nil), state.LegacyBins...)
	return nil
}

// Observe accumulates one bounded, frame-aligned block. samples holds the
// normalized floats exactly as handed to the quantizer. For noise-shaped mode
// the legacy bank records what plain round-to-nearest would have delivered from
// the same floats, which is the comparison that makes the redistribution
// auditable. In legacy mode both banks hold the delivered error.
func (accumulator *measurementAccumulator) Observe(samples []float32, quantized []int16, spec audiocodec.QuantizationSpec) {
	channels := accumulator.state.Channels
	frames := len(samples) / channels
	accumulator.state.Frames += int64(frames)
	shapeComparison := spec.Mode == audiocodec.QuantizationNoiseShaped
	size := accumulator.state.WindowFrames
	for channel := 0; channel < channels; channel++ {
		for index := range accumulator.window {
			accumulator.window[index] = 0
			accumulator.legacyW[index] = 0
		}
		for frame := 0; frame < frames && frame < size; frame++ {
			sampleIndex := frame*channels + channel
			delivered := float64(audiocodec.DecodePCM16Sample(quantized[sampleIndex])) - float64(samples[sampleIndex])
			accumulator.window[frame] = delivered
			if shapeComparison {
				legacySample := audiocodec.LegacyQuantize(samples[sampleIndex])
				accumulator.legacyW[frame] = float64(audiocodec.DecodePCM16Sample(legacySample)) - float64(samples[sampleIndex])
			} else {
				accumulator.legacyW[frame] = delivered
			}
		}
		accumulator.addSpectrum(accumulator.window, accumulator.state.ShapedBins)
		accumulator.addSpectrum(accumulator.legacyW, accumulator.state.LegacyBins)
	}
	accumulator.state.Windows++
}

func (accumulator *measurementAccumulator) addSpectrum(window []float64, bins []float64) {
	spectrum := accumulator.spectrum
	if cap(spectrum) < len(window) {
		spectrum = make([]float64, len(window))
		accumulator.spectrum = spectrum
	}
	spectrum = spectrum[:len(window)]
	imag := accumulator.imag
	if cap(imag) < len(window) {
		imag = make([]float64, len(window))
		accumulator.imag = imag
	}
	imag = imag[:len(window)]
	size := len(window)
	for index := 0; index < size; index++ {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(index)/float64(size))
		spectrum[index] = window[index] * w
		imag[index] = 0
	}
	transform(spectrum, imag)
	for bin := 0; bin <= size/2; bin++ {
		power := spectrum[bin]*spectrum[bin] + imag[bin]*imag[bin]
		if bin != 0 && bin != size/2 {
			power *= 2
		}
		bins[bin] += power
	}
}

func (accumulator *measurementAccumulator) audibleBand() (float64, float64) {
	return audiocodec.AudibleBandLowHz(), audiocodec.AudibleBandEdgeHz(accumulator.state.SampleRate)
}

func (accumulator *measurementAccumulator) bandPowerDBFS(bins []float64, low, high float64) float64 {
	if accumulator.state.Windows == 0 || len(bins) == 0 {
		return silenceFloor
	}
	size := accumulator.state.WindowFrames
	nyquist := float64(accumulator.state.SampleRate) / 2
	first := int(math.Ceil(low * float64(size) / (2 * nyquist)))
	last := int(math.Floor(high * float64(size) / (2 * nyquist)))
	if first < 0 {
		first = 0
	}
	if last >= len(bins) {
		last = len(bins) - 1
	}
	if last < first {
		return silenceFloor
	}
	sum := 0.0
	for bin := first; bin <= last; bin++ {
		sum += bins[bin]
	}
	windowEnergy := 0.0
	for index := 0; index < size; index++ {
		w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(index)/float64(size))
		windowEnergy += w * w
	}
	power := sum / float64(accumulator.state.Windows) / (float64(size) * windowEnergy) / float64(accumulator.state.Channels)
	if power <= 0 {
		return silenceFloor
	}
	value := 10 * math.Log10(power)
	if value < silenceFloor {
		return silenceFloor
	}
	return value
}

func (accumulator *measurementAccumulator) AudibleFloorDBFS() float64 {
	low, high := accumulator.audibleBand()
	return accumulator.bandPowerDBFS(accumulator.state.ShapedBins, low, high)
}

func (accumulator *measurementAccumulator) OutOfBandFloorDBFS() float64 {
	_, high := accumulator.audibleBand()
	return accumulator.bandPowerDBFS(accumulator.state.ShapedBins, high, float64(accumulator.state.SampleRate)/2)
}

func (accumulator *measurementAccumulator) LegacyAudibleFloorDBFS() float64 {
	if accumulator.state.LegacyBins == nil {
		return 0
	}
	_, ok := accumulator.legacyPresent()
	if !ok {
		return 0
	}
	low, high := accumulator.audibleBand()
	return accumulator.bandPowerDBFS(accumulator.state.LegacyBins, low, high)
}

func (accumulator *measurementAccumulator) legacyPresent() (struct{}, bool) {
	total := 0.0
	for _, value := range accumulator.state.LegacyBins {
		total += value
	}
	return struct{}{}, total > 0
}

func (accumulator *measurementAccumulator) AudibleFloorDeltaDB() float64 {
	if _, ok := accumulator.legacyPresent(); !ok {
		return 0
	}
	return accumulator.LegacyAudibleFloorDBFS() - accumulator.AudibleFloorDBFS()
}

const silenceFloor = -120.0

// transform performs an in-place radix-2 Cooley-Tukey FFT.
func transform(re, im []float64) {
	n := len(re)
	if n < 2 || n&(n-1) != 0 {
		return
	}
	j := 0
	for i := 1; i < n; i++ {
		bit := n >> 1
		for j&bit != 0 {
			j ^= bit
			bit >>= 1
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		half := length / 2
		for start := 0; start < n; start += length {
			for k := 0; k < half; k++ {
				cosine := math.Cos(angle * float64(k))
				sine := math.Sin(angle * float64(k))
				left := start + k
				right := left + half
				realPart := re[right]*cosine - im[right]*sine
				imaginaryPart := re[right]*sine + im[right]*cosine
				re[right] = re[left] - realPart
				im[right] = im[left] - imaginaryPart
				re[left] += realPart
				im[left] += imaginaryPart
			}
		}
	}
}

var _ = audiocodec.DecodePCM16Sample
