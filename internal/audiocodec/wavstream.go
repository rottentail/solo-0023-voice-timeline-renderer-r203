package audiocodec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// WAVLayout describes where the PCM payload starts inside a RIFF/WAVE file.
type WAVLayout struct {
	Info       WAVInfo
	DataOffset int64
	DataSize   int64
}

// ParseWAVLayout validates a RIFF/WAVE header without materializing samples.
// The supplied reader must expose random access to the declared file size.
func ParseWAVLayout(reader io.ReaderAt, size int64) (WAVLayout, error) {
	if size < 12 {
		return WAVLayout{}, errors.New("file is shorter than a RIFF header")
	}
	header := make([]byte, 12)
	if _, err := reader.ReadAt(header, 0); err != nil {
		return WAVLayout{}, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return WAVLayout{}, errors.New("file is not a RIFF/WAVE payload")
	}
	riffSize := uint64(binary.LittleEndian.Uint32(header[4:8]))
	if riffSize+8 > uint64(size) {
		return WAVLayout{}, errors.New("RIFF size exceeds the supplied payload")
	}

	var formatChunk []byte
	var dataOffset int64
	var dataSize int64
	seenData := false
	offset := int64(12)
	for offset+8 <= size {
		chunkHeader := make([]byte, 8)
		if _, err := reader.ReadAt(chunkHeader, offset); err != nil {
			return WAVLayout{}, err
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		if chunkSize < 0 || payloadEnd < payloadStart || payloadEnd > size {
			return WAVLayout{}, fmt.Errorf("chunk %q exceeds the payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			if chunkSize > 1<<20 {
				return WAVLayout{}, errors.New("fmt chunk is unreasonably large")
			}
			formatChunk = make([]byte, chunkSize)
			if _, err := reader.ReadAt(formatChunk, payloadStart); err != nil {
				return WAVLayout{}, err
			}
		case "data":
			dataOffset = payloadStart
			dataSize = chunkSize
			seenData = true
		}
		offset = payloadEnd
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if len(formatChunk) < 16 {
		return WAVLayout{}, errors.New("fmt chunk is missing or too short")
	}
	if !seenData {
		return WAVLayout{}, errors.New("data chunk is missing")
	}

	audioFormat := binary.LittleEndian.Uint16(formatChunk[0:2])
	channels := int(binary.LittleEndian.Uint16(formatChunk[2:4]))
	sampleRate := int(binary.LittleEndian.Uint32(formatChunk[4:8]))
	byteRate := binary.LittleEndian.Uint32(formatChunk[8:12])
	blockAlign := int(binary.LittleEndian.Uint16(formatChunk[12:14]))
	bitsPerSample := int(binary.LittleEndian.Uint16(formatChunk[14:16]))

	if audioFormat != pcmFormatCode {
		return WAVLayout{}, fmt.Errorf("unsupported WAV format code %d", audioFormat)
	}
	if bitsPerSample != pcmBitWidth {
		return WAVLayout{}, fmt.Errorf("unsupported PCM width %d", bitsPerSample)
	}
	if channels != 1 && channels != 2 {
		return WAVLayout{}, fmt.Errorf("unsupported channel count %d", channels)
	}
	if sampleRate < 8000 || sampleRate > 192000 {
		return WAVLayout{}, fmt.Errorf("sample rate %d is outside the accepted range", sampleRate)
	}
	expectedBlockAlign := channels * (bitsPerSample / 8)
	if blockAlign != expectedBlockAlign {
		return WAVLayout{}, errors.New("block alignment does not match channels and sample width")
	}
	if byteRate != uint32(sampleRate*blockAlign) {
		return WAVLayout{}, errors.New("byte rate does not match sample rate and block alignment")
	}
	if dataSize <= 0 || dataSize%int64(blockAlign) != 0 {
		return WAVLayout{}, errors.New("sample data is empty or not frame-aligned")
	}
	frameCount := dataSize / int64(blockAlign)
	return WAVLayout{
		Info: WAVInfo{
			SampleRate:    sampleRate,
			Channels:      channels,
			BitsPerSample: bitsPerSample,
			FrameCount:    frameCount,
			ByteCount:     size,
		},
		DataOffset: dataOffset,
		DataSize:   dataSize,
	}, nil
}

// ProbeWAV validates a complete payload in memory and measures its samples
// with the exact decode mapping used by DecodeWAV.
func ProbeWAV(data []byte) (WAVInfo, Metrics, error) {
	layout, err := ParseWAVLayout(sliceReaderAt(data), int64(len(data)))
	if err != nil {
		return WAVInfo{}, Metrics{}, err
	}
	metrics, err := measurePCM16Range(data, layout)
	if err != nil {
		return WAVInfo{}, Metrics{}, err
	}
	return layout.Info, metrics, nil
}

type sliceReaderAt []byte

func (slice sliceReaderAt) ReadAt(target []byte, offset int64) (int, error) {
	if offset < 0 || offset >= int64(len(slice)) {
		return 0, io.EOF
	}
	copied := copy(target, slice[offset:])
	if copied < len(target) {
		return copied, io.EOF
	}
	return copied, nil
}

func measurePCM16Range(data []byte, layout WAVLayout) (Metrics, error) {
	if layout.DataOffset < 0 || layout.DataSize < 0 || layout.DataOffset+layout.DataSize > int64(len(data)) {
		return Metrics{}, errors.New("PCM range is outside the payload")
	}
	payload := data[layout.DataOffset : layout.DataOffset+layout.DataSize]
	peak := 0.0
	sumSquares := 0.0
	for index := 0; index+1 < len(payload); index += 2 {
		value := float64(int16(binary.LittleEndian.Uint16(payload[index : index+2])))
		if value < 0 {
			value /= 32768
		} else {
			value /= 32767
		}
		absolute := value
		if absolute < 0 {
			absolute = -absolute
		}
		if absolute > peak {
			peak = absolute
		}
		sumSquares += value * value
	}
	count := len(payload) / 2
	if count == 0 {
		return Metrics{PeakDBFS: silenceFloorDBFS, RMSDBFS: silenceFloorDBFS}, nil
	}
	rms := math.Sqrt(sumSquares / float64(count))
	return Metrics{PeakDBFS: amplitudeDBFS(peak), RMSDBFS: amplitudeDBFS(rms)}, nil
}

// FrameReader streams decoded float samples from a PCM WAV source file.
type FrameReader interface {
	Info() WAVInfo
	ReadFrames(ctx context.Context, startFrame, frameCount int64, destination []float32) error
	Close() error
}

type fileFrameReader struct {
	reader     io.ReaderAt
	layout     WAVLayout
	raw        []byte
	blockAlign int
	closed     bool
}

// NewFileFrameReader builds a streaming reader over an opened WAV file.
func NewFileFrameReader(file io.ReaderAt, size int64) (FrameReader, error) {
	layout, err := ParseWAVLayout(file, size)
	if err != nil {
		return nil, err
	}
	return &fileFrameReader{
		reader:     file,
		layout:     layout,
		blockAlign: layout.Info.Channels * 2,
	}, nil
}

func (reader *fileFrameReader) Info() WAVInfo { return reader.layout.Info }

func (reader *fileFrameReader) ReadFrames(ctx context.Context, startFrame, frameCount int64, destination []float32) error {
	if reader.closed {
		return errors.New("frame reader is closed")
	}
	if startFrame < 0 || frameCount < 0 || startFrame+frameCount > reader.layout.Info.FrameCount {
		return errors.New("frame range is outside the source")
	}
	needed := frameCount * int64(reader.layout.Info.Channels)
	if int64(len(destination)) != needed {
		return errors.New("destination frame buffer does not match the request")
	}
	if frameCount == 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	byteCount := frameCount * int64(reader.blockAlign)
	if int64(cap(reader.raw)) < byteCount {
		reader.raw = make([]byte, byteCount)
	}
	pcm := reader.raw[:byteCount]
	if _, err := reader.reader.ReadAt(pcm, reader.layout.DataOffset+startFrame*int64(reader.blockAlign)); err != nil {
		reader.raw = pcm[:0]
		return err
	}
	for index := range destination {
		destination[index] = DecodePCM16Sample(int16(binary.LittleEndian.Uint16(pcm[index*2 : index*2+2])))
	}
	reader.raw = pcm[:0]
	return nil
}

func (reader *fileFrameReader) Close() error {
	if reader.closed {
		return nil
	}
	reader.closed = true
	reader.raw = nil
	return nil
}
