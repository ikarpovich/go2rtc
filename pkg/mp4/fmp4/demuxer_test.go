package fmp4

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test H.264 avcC parsing
func TestParseAVCC(t *testing.T) {
	// Minimal valid avcC box
	// configurationVersion=1, profile=66, profile_compat=0, level=30
	// lengthSizeMinusOne=3 (4 bytes), numSPS=1
	avcC := []byte{
		0x01,       // configurationVersion
		0x42,       // AVCProfileIndication (Baseline)
		0x00,       // profile_compatibility
		0x1E,       // AVCLevelIndication (3.0)
		0xFF,       // lengthSizeMinusOne (0xFF & 0x03 = 3, meaning 4 bytes)
		0xE1,       // numOfSequenceParameterSets (0xE1 & 0x1F = 1)
		0x00, 0x08, // SPS length = 8
		0x67, 0x42, 0x00, 0x1E, 0x8D, 0x8D, 0x40, 0x50, // SPS data
		0x01,       // numOfPictureParameterSets = 1
		0x00, 0x04, // PPS length = 4
		0x68, 0xCE, 0x3C, 0x80, // PPS data
	}

	d := NewDemuxer()
	err := d.parseAVCC(avcC)
	require.NoError(t, err)

	assert.Equal(t, []byte{0x67, 0x42, 0x00, 0x1E, 0x8D, 0x8D, 0x40, 0x50}, d.SPS)
	assert.Equal(t, []byte{0x68, 0xCE, 0x3C, 0x80}, d.PPS)
}

func TestParseAVCCErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"too short", []byte{0x01, 0x42, 0x00, 0x1E}},
		{"truncated SPS", []byte{0x01, 0x42, 0x00, 0x1E, 0xFF, 0xE1, 0x00, 0x08, 0x67}},
		{"missing PPS count", []byte{0x01, 0x42, 0x00, 0x1E, 0xFF, 0xE1, 0x00, 0x01, 0x67}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewDemuxer()
			err := d.parseAVCC(tt.data)
			assert.Error(t, err)
		})
	}
}

// Test NALU extraction from length-prefixed format
func TestExtractNALUs(t *testing.T) {
	d := NewDemuxer()
	d.VideoCodec = "h264"
	d.SPS = []byte{0x67, 0x42, 0x00, 0x1E}
	d.PPS = []byte{0x68, 0xCE, 0x3C, 0x80}

	// Sample with two NALUs: length-prefixed format
	// [4-byte length][NALU 1][4-byte length][NALU 2]
	sample := []byte{
		0x00, 0x00, 0x00, 0x05, // length = 5
		0x65, 0x01, 0x02, 0x03, 0x04, // NALU 1 (IDR slice)
		0x00, 0x00, 0x00, 0x03, // length = 3
		0x41, 0x05, 0x06, // NALU 2 (non-IDR slice)
	}

	// Non-keyframe: should only return the NALUs from sample
	nalus, err := d.extractNALUs(sample, false)
	require.NoError(t, err)
	assert.Len(t, nalus, 2)
	assert.Equal(t, []byte{0x65, 0x01, 0x02, 0x03, 0x04}, nalus[0])
	assert.Equal(t, []byte{0x41, 0x05, 0x06}, nalus[1])

	// Keyframe: should prepend SPS/PPS
	nalus, err = d.extractNALUs(sample, true)
	require.NoError(t, err)
	assert.Len(t, nalus, 4) // SPS + PPS + 2 NALUs
	assert.Equal(t, d.SPS, nalus[0])
	assert.Equal(t, d.PPS, nalus[1])
	assert.Equal(t, []byte{0x65, 0x01, 0x02, 0x03, 0x04}, nalus[2])
	assert.Equal(t, []byte{0x41, 0x05, 0x06}, nalus[3])
}

func TestExtractNALUsErrors(t *testing.T) {
	d := NewDemuxer()
	d.VideoCodec = "h264"

	tests := []struct {
		name   string
		sample []byte
	}{
		{"length exceeds sample", []byte{0x00, 0x00, 0x00, 0x10, 0x65, 0x01}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.extractNALUs(tt.sample, false)
			assert.Error(t, err)
		})
	}
}

// Test GetAnnexB conversion
func TestGetAnnexB(t *testing.T) {
	d := NewDemuxer()
	d.VideoCodec = "h264"
	d.SPS = []byte{0x67, 0x42, 0x00, 0x1E}
	d.PPS = []byte{0x68, 0xCE, 0x3C, 0x80}

	annexB := d.GetAnnexB()

	// Should contain start codes + SPS + start code + PPS
	expected := []byte{
		0x00, 0x00, 0x00, 0x01, // start code
		0x67, 0x42, 0x00, 0x1E, // SPS
		0x00, 0x00, 0x00, 0x01, // start code
		0x68, 0xCE, 0x3C, 0x80, // PPS
	}

	assert.Equal(t, expected, annexB)
}

// Test with empty demuxer
func TestDemuxerEmpty(t *testing.T) {
	d := NewDemuxer()

	// SetInit with no data should fail
	err := d.SetInit([]byte{})
	assert.Error(t, err)

	// Demux with no callback should not error
	err = d.Demux([]byte{})
	assert.NoError(t, err)
}

// Test callback invocation
func TestDemuxerCallback(t *testing.T) {
	d := NewDemuxer()
	d.VideoCodec = "h264"
	d.trackID = 1
	d.timeScale = 90000

	callbackCalled := false
	d.SetOnFrame(func(nalus [][]byte, keyframe bool, pts, dts uint64) {
		callbackCalled = true
		assert.NotEmpty(t, nalus)
	})

	// Note: This test would need real fMP4 fragment data to fully test
	// For now, just verify the callback mechanism works
	assert.NotNil(t, d.onFrame)
	_ = callbackCalled // will be true when real fragment data is provided
}

func TestNALUsToAnnexB(t *testing.T) {
	nalus := [][]byte{
		{0x67, 0x42, 0x00, 0x1E}, // SPS
		{0x68, 0xCE, 0x3C, 0x80}, // PPS
		{0x65, 0x01, 0x02, 0x03}, // IDR slice
	}

	result := NALUsToAnnexB(nalus)

	// Should have start codes before each NALU
	assert.Contains(t, result, byte(0x00))
	assert.Contains(t, result, byte(0x01))
	assert.Contains(t, result, byte(0x67)) // First byte of SPS
	assert.Contains(t, result, byte(0x68)) // First byte of PPS
	assert.Contains(t, result, byte(0x65)) // First byte of IDR
}

// Benchmark NALU extraction
func BenchmarkExtractNALUs(b *testing.B) {
	d := NewDemuxer()
	d.VideoCodec = "h264"

	// Create a sample with 10 NALUs
	sample := make([]byte, 0, 1000)
	for i := 0; i < 10; i++ {
		sample = append(sample, 0x00, 0x00, 0x00, 0x10) // length = 16
		for j := 0; j < 16; j++ {
			sample = append(sample, byte(i*16+j))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = d.extractNALUs(sample, false)
	}
}

func TestHexDump(t *testing.T) {
	// Helper test to verify hex encoding works (used in debugging)
	data := []byte{0x00, 0x01, 0x02, 0x03}
	hexStr := hex.EncodeToString(data)
	assert.Equal(t, "00010203", hexStr)
}
