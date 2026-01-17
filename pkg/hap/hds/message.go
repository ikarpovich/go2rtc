package hds

import (
	"errors"
	"fmt"
)

// MessageType represents the type of HDS message
type MessageType byte

const (
	MessageEvent    MessageType = 1
	MessageRequest  MessageType = 2
	MessageResponse MessageType = 3
)

// Protocol constants
const (
	ProtocolControl       = "control"
	ProtocolTargetControl = "targetControl"
	ProtocolDataSend      = "dataSend"
)

// Topic constants
const (
	TopicHello  = "hello"
	TopicWhoami = "whoami"
	TopicOpen   = "open"
	TopicData   = "data"
	TopicAck    = "ack"
	TopicClose  = "close"
)

// DataType constants for dataSend.data events
const (
	DataTypeMediaInit     = 1 // MEDIA_INITIALIZATION
	DataTypeMediaFragment = 2 // MEDIA_FRAGMENT
)

var (
	ErrInvalidMessage = errors.New("invalid message format")
	ErrMissingField   = errors.New("missing required field")
)

// Message represents an HDS protocol message
type Message struct {
	Type     MessageType
	Protocol string
	Topic    string
	ID       int64          // for request/response
	Status   int64          // for response
	Body     map[string]any // message body
}

// Header represents the HDS message header
type Header struct {
	Protocol string
	Topic    string
	ID       int64 // for request/response
}

// IsEvent returns true if this is an event message
func (m *Message) IsEvent() bool {
	return m.Type == MessageEvent
}

// IsRequest returns true if this is a request message
func (m *Message) IsRequest() bool {
	return m.Type == MessageRequest
}

// IsResponse returns true if this is a response message
func (m *Message) IsResponse() bool {
	return m.Type == MessageResponse
}

// ParseMessage parses a raw message payload into a Message
func ParseMessage(data []byte) (*Message, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("%w: missing header length", ErrInvalidMessage)
	}

	// First byte is header length (uint8), followed by header and body payloads.
	headerLen := int(data[0])
	if len(data) < 1+headerLen {
		return nil, fmt.Errorf("%w: header length exceeds payload", ErrInvalidMessage)
	}

	// Parse header dict
	headerData := data[1 : 1+headerLen]
	headerReader := NewReader(headerData)
	headerVal, err := headerReader.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to read header: %w", err)
	}

	header, ok := headerVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: header must be dictionary", ErrInvalidMessage)
	}

	// Parse message body (remaining bytes)
	bodyData := data[1+headerLen:]
	bodyReader := NewReader(bodyData)
	bodyVal, err := bodyReader.Decode()
	if err != nil {
		return nil, fmt.Errorf("failed to read body: %w", err)
	}

	body, ok := bodyVal.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: body must be dictionary", ErrInvalidMessage)
	}

	// Build Message
	msg := &Message{
		Body: body,
	}

	// Parse header fields
	if protocol, ok := header["protocol"].(string); ok {
		msg.Protocol = protocol
	} else {
		return nil, fmt.Errorf("%w: protocol", ErrMissingField)
	}

	// Determine message type and topic
	if event, ok := header["event"].(string); ok {
		msg.Type = MessageEvent
		msg.Topic = event
	} else if request, ok := header["request"].(string); ok {
		msg.Type = MessageRequest
		msg.Topic = request
		if id, ok := header["id"].(int64); ok {
			msg.ID = id
		} else {
			return nil, fmt.Errorf("%w: id for request", ErrMissingField)
		}
	} else if response, ok := header["response"].(string); ok {
		msg.Type = MessageResponse
		msg.Topic = response
		if id, ok := header["id"].(int64); ok {
			msg.ID = id
		} else {
			return nil, fmt.Errorf("%w: id for response", ErrMissingField)
		}
		if status, ok := header["status"].(int64); ok {
			msg.Status = status
		}
	} else {
		return nil, fmt.Errorf("%w: missing event/request/response", ErrMissingField)
	}

	return msg, nil
}

// Marshal serializes a Message to binary format
func (m *Message) Marshal() ([]byte, error) {
	w := NewWriter()

	// Build header dict
	header := make(map[string]any)
	header["protocol"] = m.Protocol

	switch m.Type {
	case MessageEvent:
		header["event"] = m.Topic
	case MessageRequest:
		header["request"] = m.Topic
		header["id"] = m.ID
	case MessageResponse:
		header["response"] = m.Topic
		header["id"] = m.ID
		header["status"] = m.Status
	default:
		return nil, fmt.Errorf("invalid message type: %d", m.Type)
	}

	// Encode header and body with DataStream encoding.
	headerWriter := NewWriter()
	if err := headerWriter.Encode(header); err != nil {
		return nil, fmt.Errorf("failed to encode header: %w", err)
	}
	headerBytes := headerWriter.Bytes()
	if len(headerBytes) > 0xFF {
		return nil, fmt.Errorf("header too long: %d", len(headerBytes))
	}

	bodyWriter := NewWriter()
	if err := bodyWriter.Encode(m.Body); err != nil {
		return nil, fmt.Errorf("failed to encode body: %w", err)
	}
	bodyBytes := bodyWriter.Bytes()

	// Payload format: 1-byte header length + header bytes + body bytes.
	w.buf = append(w.buf, byte(len(headerBytes)))
	w.buf = append(w.buf, headerBytes...)
	w.buf = append(w.buf, bodyBytes...)

	return w.Bytes(), nil
}

// NewEvent creates a new event message
func NewEvent(protocol, topic string, body map[string]any) *Message {
	if body == nil {
		body = make(map[string]any)
	}
	return &Message{
		Type:     MessageEvent,
		Protocol: protocol,
		Topic:    topic,
		Body:     body,
	}
}

// NewRequest creates a new request message
func NewRequest(protocol, topic string, id int64, body map[string]any) *Message {
	if body == nil {
		body = make(map[string]any)
	}
	return &Message{
		Type:     MessageRequest,
		Protocol: protocol,
		Topic:    topic,
		ID:       id,
		Body:     body,
	}
}

// NewResponse creates a new response message
func NewResponse(protocol, topic string, id, status int64, body map[string]any) *Message {
	if body == nil {
		body = make(map[string]any)
	}
	return &Message{
		Type:     MessageResponse,
		Protocol: protocol,
		Topic:    topic,
		ID:       id,
		Status:   status,
		Body:     body,
	}
}

// DataSendMetadata represents metadata for dataSend.data packets
type DataSendMetadata struct {
	DataType                int64
	DataSequenceNumber      int64
	DataChunkSequenceNumber int64
	IsLastDataChunk         bool
	DataTotalSize           int64
}

// ParseDataSendMetadata extracts metadata from a packet metadata dict
func ParseDataSendMetadata(metadata map[string]any) *DataSendMetadata {
	m := &DataSendMetadata{}

	if v, ok := metadata["dataType"]; ok {
		switch t := v.(type) {
		case int64:
			m.DataType = t
		case int:
			m.DataType = int64(t)
		case float64:
			m.DataType = int64(t)
		case string:
			switch t {
			case "mediaInitialization":
				m.DataType = DataTypeMediaInit
			case "mediaFragment":
				m.DataType = DataTypeMediaFragment
			}
		}
	}
	if v, ok := metadata["dataSequenceNumber"]; ok {
		m.DataSequenceNumber = toInt64(v)
	}
	if v, ok := metadata["dataChunkSequenceNumber"]; ok {
		m.DataChunkSequenceNumber = toInt64(v)
	}
	if v, ok := metadata["isLastDataChunk"]; ok {
		switch b := v.(type) {
		case bool:
			m.IsLastDataChunk = b
		case string:
			m.IsLastDataChunk = b == "true"
		}
	}
	if v, ok := metadata["dataTotalSize"]; ok {
		m.DataTotalSize = toInt64(v)
	}

	return m
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case float64:
		return int64(t)
	}
	return 0
}
