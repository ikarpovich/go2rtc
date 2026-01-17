package hds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageEvent(t *testing.T) {
	body := map[string]any{
		"streamId":    int64(1),
		"endOfStream": false,
	}

	msg := NewEvent(ProtocolDataSend, TopicData, body)

	assert.Equal(t, MessageEvent, msg.Type)
	assert.Equal(t, ProtocolDataSend, msg.Protocol)
	assert.Equal(t, TopicData, msg.Topic)
	assert.True(t, msg.IsEvent())
	assert.False(t, msg.IsRequest())
	assert.False(t, msg.IsResponse())
}

func TestMessageRequest(t *testing.T) {
	body := map[string]any{
		"target": "controller",
		"type":   "camera.recording",
	}

	msg := NewRequest(ProtocolDataSend, TopicOpen, 12345, body)

	assert.Equal(t, MessageRequest, msg.Type)
	assert.Equal(t, ProtocolDataSend, msg.Protocol)
	assert.Equal(t, TopicOpen, msg.Topic)
	assert.Equal(t, int64(12345), msg.ID)
	assert.False(t, msg.IsEvent())
	assert.True(t, msg.IsRequest())
	assert.False(t, msg.IsResponse())
}

func TestMessageResponse(t *testing.T) {
	body := map[string]any{
		"streamId": int64(1),
	}

	msg := NewResponse(ProtocolDataSend, TopicOpen, 12345, 0, body)

	assert.Equal(t, MessageResponse, msg.Type)
	assert.Equal(t, ProtocolDataSend, msg.Protocol)
	assert.Equal(t, TopicOpen, msg.Topic)
	assert.Equal(t, int64(12345), msg.ID)
	assert.Equal(t, int64(0), msg.Status)
	assert.False(t, msg.IsEvent())
	assert.False(t, msg.IsRequest())
	assert.True(t, msg.IsResponse())
}

func TestMessageMarshalEvent(t *testing.T) {
	msg := NewEvent(ProtocolControl, TopicHello, map[string]any{
		"version": int64(1),
	})

	data, err := msg.Marshal()
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Parse it back
	parsed, err := ParseMessage(data)
	require.NoError(t, err)
	assert.Equal(t, msg.Type, parsed.Type)
	assert.Equal(t, msg.Protocol, parsed.Protocol)
	assert.Equal(t, msg.Topic, parsed.Topic)
	assert.Equal(t, msg.Body, parsed.Body)
}

func TestMessageMarshalRequest(t *testing.T) {
	msg := NewRequest(ProtocolDataSend, TopicOpen, 999, map[string]any{
		"target": "controller",
		"type":   "camera.recording",
	})

	data, err := msg.Marshal()
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Parse it back
	parsed, err := ParseMessage(data)
	require.NoError(t, err)
	assert.Equal(t, msg.Type, parsed.Type)
	assert.Equal(t, msg.Protocol, parsed.Protocol)
	assert.Equal(t, msg.Topic, parsed.Topic)
	assert.Equal(t, msg.ID, parsed.ID)
	assert.Equal(t, msg.Body, parsed.Body)
}

func TestMessageMarshalResponse(t *testing.T) {
	msg := NewResponse(ProtocolDataSend, TopicOpen, 999, 0, map[string]any{
		"streamId": int64(1),
	})

	data, err := msg.Marshal()
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	// Parse it back
	parsed, err := ParseMessage(data)
	require.NoError(t, err)
	assert.Equal(t, msg.Type, parsed.Type)
	assert.Equal(t, msg.Protocol, parsed.Protocol)
	assert.Equal(t, msg.Topic, parsed.Topic)
	assert.Equal(t, msg.ID, parsed.ID)
	assert.Equal(t, msg.Status, parsed.Status)
	assert.Equal(t, msg.Body, parsed.Body)
}

func TestMessageParseDataSendEvent(t *testing.T) {
	// Simulate a dataSend.data event
	msg := NewEvent(ProtocolDataSend, TopicData, map[string]any{
		"streamId": int64(1),
		"packets": []any{
			map[string]any{
				"data": []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70}, // ftyp box
				"metadata": map[string]any{
					"dataType":                int64(DataTypeMediaInit),
					"dataSequenceNumber":      int64(0),
					"dataChunkSequenceNumber": int64(0),
					"isLastDataChunk":         true,
					"dataTotalSize":           int64(1024),
				},
			},
		},
		"endOfStream": false,
	})

	data, err := msg.Marshal()
	require.NoError(t, err)

	parsed, err := ParseMessage(data)
	require.NoError(t, err)

	assert.Equal(t, ProtocolDataSend, parsed.Protocol)
	assert.Equal(t, TopicData, parsed.Topic)
	assert.True(t, parsed.IsEvent())

	// Extract packets
	packets, ok := parsed.Body["packets"].([]any)
	require.True(t, ok)
	require.Len(t, packets, 1)

	packet, ok := packets[0].(map[string]any)
	require.True(t, ok)

	// Extract metadata
	metadata, ok := packet["metadata"].(map[string]any)
	require.True(t, ok)

	meta := ParseDataSendMetadata(metadata)
	assert.Equal(t, int64(DataTypeMediaInit), meta.DataType)
	assert.Equal(t, int64(0), meta.DataSequenceNumber)
	assert.Equal(t, int64(0), meta.DataChunkSequenceNumber)
	assert.True(t, meta.IsLastDataChunk)
	assert.Equal(t, int64(1024), meta.DataTotalSize)
}

func TestMessageParseHelloEvent(t *testing.T) {
	// Simulate a control.hello event
	msg := NewEvent(ProtocolControl, TopicHello, map[string]any{
		"version": int64(1),
	})

	data, err := msg.Marshal()
	require.NoError(t, err)

	parsed, err := ParseMessage(data)
	require.NoError(t, err)

	assert.Equal(t, ProtocolControl, parsed.Protocol)
	assert.Equal(t, TopicHello, parsed.Topic)
	assert.True(t, parsed.IsEvent())

	version, ok := parsed.Body["version"].(int64)
	require.True(t, ok)
	assert.Equal(t, int64(1), version)
}

func TestParseDataSendMetadata(t *testing.T) {
	metadata := map[string]any{
		"dataType":                int64(DataTypeMediaFragment),
		"dataSequenceNumber":      int64(42),
		"dataChunkSequenceNumber": int64(5),
		"isLastDataChunk":         false,
		"dataTotalSize":           int64(16384),
	}

	meta := ParseDataSendMetadata(metadata)

	assert.Equal(t, int64(DataTypeMediaFragment), meta.DataType)
	assert.Equal(t, int64(42), meta.DataSequenceNumber)
	assert.Equal(t, int64(5), meta.DataChunkSequenceNumber)
	assert.False(t, meta.IsLastDataChunk)
	assert.Equal(t, int64(16384), meta.DataTotalSize)
}

func TestParseDataSendMetadataPartial(t *testing.T) {
	// Test with missing fields (should use zero values)
	metadata := map[string]any{
		"dataType": int64(1),
	}

	meta := ParseDataSendMetadata(metadata)

	assert.Equal(t, int64(1), meta.DataType)
	assert.Equal(t, int64(0), meta.DataSequenceNumber)
	assert.Equal(t, int64(0), meta.DataChunkSequenceNumber)
	assert.False(t, meta.IsLastDataChunk)
	assert.Equal(t, int64(0), meta.DataTotalSize)
}

func TestMessageParseErrors(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}},
		{"invalid header length", []byte{0xFF}},
		{"missing protocol", func() []byte {
			w := NewWriter()
			w.Encode(int64(1)) // header length
			w.Encode(map[string]any{"event": "test"})
			w.Encode(map[string]any{})
			return w.Bytes()
		}()},
		{"missing event/request/response", func() []byte {
			w := NewWriter()
			headerW := NewWriter()
			headerW.Encode(map[string]any{"protocol": "control"})
			headerBytes := headerW.Bytes()
			w.Encode(int64(len(headerBytes)))
			w.buf = append(w.buf, headerBytes...)
			w.Encode(map[string]any{})
			return w.Bytes()
		}()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMessage(tt.data)
			assert.Error(t, err)
		})
	}
}

func TestMessageRoundTrip(t *testing.T) {
	messages := []*Message{
		NewEvent(ProtocolControl, TopicHello, map[string]any{"version": int64(1)}),
		NewRequest(ProtocolDataSend, TopicOpen, 123, map[string]any{"target": "controller"}),
		NewResponse(ProtocolDataSend, TopicOpen, 123, 0, map[string]any{"streamId": int64(1)}),
		NewEvent(ProtocolDataSend, TopicData, map[string]any{
			"streamId": int64(1),
			"packets": []any{
				map[string]any{
					"data": []byte{0x01, 0x02, 0x03},
					"metadata": map[string]any{
						"dataType":           int64(2),
						"dataSequenceNumber": int64(42),
					},
				},
			},
		}),
	}

	for i, msg := range messages {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			data, err := msg.Marshal()
			require.NoError(t, err)

			parsed, err := ParseMessage(data)
			require.NoError(t, err)

			assert.Equal(t, msg.Type, parsed.Type)
			assert.Equal(t, msg.Protocol, parsed.Protocol)
			assert.Equal(t, msg.Topic, parsed.Topic)
			assert.Equal(t, msg.ID, parsed.ID)
			assert.Equal(t, msg.Status, parsed.Status)
		})
	}
}
