package hds

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParserBooleans(t *testing.T) {
	w := NewWriter()

	require.NoError(t, w.Encode(true))
	require.NoError(t, w.Encode(false))
	require.NoError(t, w.Encode(nil))

	r := NewReader(w.Bytes())

	val, err := r.Decode()
	require.NoError(t, err)
	assert.Equal(t, true, val)

	val, err = r.Decode()
	require.NoError(t, err)
	assert.Equal(t, false, val)

	val, err = r.Decode()
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestParserIntegers(t *testing.T) {
	tests := []struct {
		name string
		val  int64
	}{
		{"minus one", -1},
		{"zero", 0},
		{"small positive", 15},
		{"max inline", 38},
		{"int8 min", -128},
		{"int8 max", 127},
		{"int16 min", -32768},
		{"int16 max", 32767},
		{"int32 min", -2147483648},
		{"int32 max", 2147483647},
		{"large", 1234567890},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.Equal(t, tt.val, val)
		})
	}
}

func TestParserFloats(t *testing.T) {
	tests := []struct {
		name string
		val  float64
	}{
		{"zero", 0.0},
		{"pi", 3.14159265359},
		{"negative", -123.456},
		{"large", 1234567890.123456},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.InDelta(t, tt.val, val, 0.0001)
		})
	}
}

func TestParserStrings(t *testing.T) {
	tests := []struct {
		name string
		val  string
	}{
		{"empty", ""},
		{"short", "hello"},
		{"inline max", "this is exactly 32 characters!!"},
		{"medium", "this string is longer than 32 characters and requires length encoding"},
		{"unicode", "Hello 世界 🌍"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.Equal(t, tt.val, val)
		})
	}
}

func TestParserData(t *testing.T) {
	tests := []struct {
		name string
		val  []byte
	}{
		{"empty", []byte{}},
		{"small", []byte{1, 2, 3, 4, 5}},
		{"inline max", make([]byte, 32)},
		{"medium", make([]byte, 256)},
		{"large", make([]byte, 10000)},
	}

	for i := range tests {
		// Fill with pattern
		for j := range tests[i].val {
			tests[i].val[j] = byte(j % 256)
		}
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.Equal(t, tt.val, val)
		})
	}
}

func TestParserArrays(t *testing.T) {
	tests := []struct {
		name string
		val  []any
	}{
		{"empty", []any{}},
		{"numbers", []any{int64(1), int64(2), int64(3)}},
		{"mixed", []any{int64(42), "hello", true, nil}},
		{"nested", []any{int64(1), []any{int64(2), int64(3)}, int64(4)}},
		{"large", make([]any, 20)}, // > 14 to test terminated encoding
	}

	// Fill large array
	for i := range tests[4].val {
		tests[4].val[i] = int64(i)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.Equal(t, tt.val, val)
		})
	}
}

func TestParserDictionaries(t *testing.T) {
	tests := []struct {
		name string
		val  map[string]any
	}{
		{"empty", map[string]any{}},
		{"simple", map[string]any{"foo": int64(42), "bar": "hello"}},
		{"nested", map[string]any{
			"level1": map[string]any{
				"level2": int64(123),
			},
		}},
		{"mixed types", map[string]any{
			"int":    int64(42),
			"string": "hello",
			"bool":   true,
			"null":   nil,
			"array":  []any{int64(1), int64(2), int64(3)},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWriter()
			require.NoError(t, w.Encode(tt.val))

			r := NewReader(w.Bytes())
			val, err := r.Decode()
			require.NoError(t, err)
			assert.Equal(t, tt.val, val)
		})
	}
}

func TestParserDates(t *testing.T) {
	// Test date encoding/decoding
	now := time.Now()
	w := NewWriter()
	require.NoError(t, w.Encode(now))

	r := NewReader(w.Bytes())
	val, err := r.Decode()
	require.NoError(t, err)

	decoded, ok := val.(time.Time)
	require.True(t, ok)

	// Compare with 1 second precision due to float conversion
	assert.InDelta(t, now.Unix(), decoded.Unix(), 1)
}

func TestParserComplexMessage(t *testing.T) {
	// Test a complex message similar to HDS protocol
	msg := map[string]any{
		"protocol": "dataSend",
		"event":    "data",
		"message": map[string]any{
			"streamId": int64(1),
			"packets": []any{
				map[string]any{
					"data": []byte{0x00, 0x01, 0x02, 0x03},
					"metadata": map[string]any{
						"dataType":                int64(2),
						"dataSequenceNumber":      int64(42),
						"dataChunkSequenceNumber": int64(1),
						"isLastDataChunk":         true,
						"dataTotalSize":           int64(4),
					},
				},
			},
			"endOfStream": false,
		},
	}

	w := NewWriter()
	require.NoError(t, w.Encode(msg))

	r := NewReader(w.Bytes())
	val, err := r.Decode()
	require.NoError(t, err)

	decoded, ok := val.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "dataSend", decoded["protocol"])
	assert.Equal(t, "data", decoded["event"])
}

func TestParserErrorCases(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"invalid tag", []byte{0xFF}},
		{"truncated int16", []byte{TagInt16, 0x01}}, // missing byte
		{"truncated string", []byte{TagUTF8Len8, 0x05, 'a', 'b'}}, // claims 5 bytes, has 2
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(tt.data)
			_, err := r.Decode()
			assert.Error(t, err)
		})
	}
}

func TestParserRoundTrip(t *testing.T) {
	// Test multiple encode/decode cycles
	values := []any{
		int64(42),
		"hello world",
		true,
		[]byte{1, 2, 3, 4, 5},
		[]any{int64(1), int64(2), int64(3)},
		map[string]any{"foo": int64(123), "bar": "test"},
	}

	w := NewWriter()
	for _, val := range values {
		require.NoError(t, w.Encode(val))
	}

	r := NewReader(w.Bytes())
	for i, expected := range values {
		val, err := r.Decode()
		require.NoError(t, err, "decode failed at index %d", i)
		assert.Equal(t, expected, val, "mismatch at index %d", i)
	}
}
