package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Audio waveform peaks for the Content Studio timeline. We decode each
// audio-bearing source to mono 8 kHz PCM and bucket it into 50 min/max pairs
// per second, quantized to int8. The client renders mirrored bars from this
// (clip-waveform.tsx) and never needs the raw audio.

const (
	studioPeaksSampleRate    = 8000
	studioPeaksBucketsPerSec = 50
	studioPeaksVersion       = 1
	// samplesPerBucket = sampleRate / bucketsPerSecond = 160.
	studioPeaksSamplesPerBucket = studioPeaksSampleRate / studioPeaksBucketsPerSec
)

// StudioPeaks is the JSON payload served at /api/studio/assets/:id/peaks.
// Peaks is a flat [min0,max0,min1,max1,…] list of int8 values (−127..127).
type StudioPeaks struct {
	Version          int    `json:"version"`
	BucketsPerSecond int    `json:"bucketsPerSecond"`
	Length           int    `json:"length"` // number of buckets
	Peaks            []int8 `json:"peaks"`
}

// peaksMaxBytes bounds the captured PCM defensively: ~totalSeconds of 8 kHz
// mono s16le when the duration is known, otherwise a 4-hour hard ceiling.
func peaksMaxBytes(totalSeconds float64) int64 {
	const bytesPerSec = studioPeaksSampleRate * 2
	const hardCeil int64 = bytesPerSec * 4 * 3600 // 4h
	if totalSeconds > 0 {
		est := int64((totalSeconds + 2) * bytesPerSec)
		if est > 0 && est < hardCeil {
			return est
		}
	}
	return hardCeil
}

// quantizePeak maps an int16 PCM sample to int8 (−127..127), rounding so that
// full-scale samples reach the rails (±32767/8 → ±127).
func quantizePeak(s int16) int8 {
	v := int(math.Round(float64(s) * 127.0 / 32768.0))
	if v > 127 {
		v = 127
	}
	if v < -127 {
		v = -127
	}
	return int8(v)
}

// buildPeaksJSON buckets little-endian mono s16le PCM into the StudioPeaks
// payload. Pure + unit-tested.
func buildPeaksJSON(pcm []byte) []byte {
	sampleCount := len(pcm) / 2
	bucketCount := (sampleCount + studioPeaksSamplesPerBucket - 1) / studioPeaksSamplesPerBucket
	if bucketCount < 0 {
		bucketCount = 0
	}
	peaks := make([]int8, 0, bucketCount*2)
	for b := 0; b < bucketCount; b++ {
		start := b * studioPeaksSamplesPerBucket
		end := start + studioPeaksSamplesPerBucket
		if end > sampleCount {
			end = sampleCount
		}
		var mn, mx int16
		first := true
		for i := start; i < end; i++ {
			s := int16(binary.LittleEndian.Uint16(pcm[i*2:]))
			if first {
				mn, mx, first = s, s, false
				continue
			}
			if s < mn {
				mn = s
			}
			if s > mx {
				mx = s
			}
		}
		peaks = append(peaks, quantizePeak(mn), quantizePeak(mx))
	}
	out, _ := json.Marshal(StudioPeaks{
		Version:          studioPeaksVersion,
		BucketsPerSecond: studioPeaksBucketsPerSec,
		Length:           bucketCount,
		Peaks:            peaks,
	})
	return out
}

// GeneratePeaksBytes decodes inputPath to mono PCM and returns the serialized
// StudioPeaks JSON. Errors are returned (the caller decides if they're fatal).
func (s *StudioIngestService) GeneratePeaksBytes(ctx context.Context, jobID, inputPath string, totalSeconds float64) ([]byte, error) {
	args := []string{
		"-y", "-i", inputPath, "-vn",
		"-ac", "1",
		"-ar", strconv.Itoa(studioPeaksSampleRate),
		"-f", "s16le", "-",
	}
	pcm, err := runStudioFFmpegCapture(ctx, s.jobManager, jobID, s.cfg.ContentStudioGPUIndex, totalSeconds, peaksMaxBytes(totalSeconds), args...)
	if err != nil {
		return nil, fmt.Errorf("decode audio for peaks: %w", err)
	}
	if len(pcm) < 2 {
		return nil, fmt.Errorf("no audio samples decoded")
	}
	return buildPeaksJSON(pcm), nil
}

// UploadPeaks stores the peaks JSON beside the original under
// <keyDir>/<assetID>_peaks.json and returns its S3 key.
func (s *StudioIngestService) UploadPeaks(ctx context.Context, originalKey, assetID string, data []byte) (string, error) {
	if s.s3Client == nil {
		return "", fmt.Errorf("S3 client not configured")
	}
	key := s3KeyDir(originalKey) + "/" + assetID + "_peaks.json"
	if _, err := s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.S3Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	}); err != nil {
		return "", err
	}
	return key, nil
}
