package homekit

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/hap"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
	"github.com/AlexxIT/go2rtc/pkg/hap/hds"
	"github.com/AlexxIT/go2rtc/pkg/mp4/fmp4"
	"github.com/pion/rtp"
)

// HDSProducer handles video streaming from HDS (HomeKit Data Stream) cameras
type HDSProducer struct {
	client *Client

	hdsConn *hds.Conn
	demuxer *fmp4.Demuxer

	videoTrack *core.Receiver

	requestID atomic.Int64
	helloID   int64

	loggedData bool

	pendingPayload []byte
	pendingPTS     uint64
	pendingKey     bool
	pendingHasSPS  bool
	pendingHasPPS  bool
	loggedKeyframe bool
}

// startHDS initiates HDS streaming mode
func (c *Client) startHDS() error {
	log.Printf("[homekit] using HDS producer mode")

	acc, err := c.hap.GetFirstAccessory()
	if err != nil {
		return fmt.Errorf("failed to get accessory: %w", err)
	}

	// Check for HDS support and read supported configurations
	char := acc.GetCharacter(camera.TypeSupportedDataStreamTransportConfiguration)
	if char == nil {
		return fmt.Errorf("accessory does not support HDS")
	}

	// Read supported transport configurations
	var supportedConfig camera.SupportedDataStreamTransportConfiguration
	if err := char.ReadTLV8(&supportedConfig); err != nil {
		log.Printf("[homekit] HDS: warning - failed to read supported configs: %v", err)
	} else {
		log.Printf("[homekit] HDS: camera supports %d transport configuration(s)", len(supportedConfig.Configs))
		for i, cfg := range supportedConfig.Configs {
			log.Printf("[homekit] HDS: config[%d] transport type=%d", i, cfg.TransportType)
		}
	}

	// Setup HDS session
	producer := &HDSProducer{
		client: c,
	}

	// Get video track
	producer.videoTrack = c.trackByKind(core.KindVideo)
	if producer.videoTrack == nil {
		return fmt.Errorf("no video track configured")
	}

	log.Printf("[homekit] HDS: video track codec=%s", producer.videoTrack.Codec.Name)
	producer.videoTrack.Codec.PayloadType = core.PayloadTypeRAW

	// Setup fMP4 demuxer
	producer.demuxer = fmp4.NewDemuxer()
	producer.demuxer.SetOnFrame(producer.handleVideoFrame)

	// Setup HDS transport
	if err := producer.setupHDSTransport(); err != nil {
		return fmt.Errorf("failed to setup HDS transport: %w", err)
	}

	// Send hello and wait for the response before requesting a stream.
	if err := producer.sendHello(); err != nil {
		return fmt.Errorf("failed to send hello: %w", err)
	}

	// Process HDS messages (will request stream after hello)
	return producer.processMessages()
}

// setupHDSTransport sets up the HDS TCP connection
func (p *HDSProducer) setupHDSTransport() error {
	acc, err := p.client.hap.GetFirstAccessory()
	if err != nil {
		return err
	}

	char := acc.GetCharacter(camera.TypeSetupDataStreamTransport)
	if char == nil {
		return fmt.Errorf("no SetupDataStreamTransport characteristic")
	}

	// Try to read current state of the characteristic
	log.Printf("[homekit] HDS: current char.Value: %v", char.Value)
	if char.Value != nil {
		var currentState camera.SetupDataStreamTransportResponse
		if err := char.ReadTLV8(&currentState); err == nil {
			log.Printf("[homekit] HDS: detected existing session - Status=%d, Port=%d",
				currentState.Status, currentState.TransportTypeSessionParameters.TCPListeningPort)
		}
	}

	// Generate controller key salt
	controllerSalt := core.RandString(32, 0)

	// Create request
	req := camera.SetupDataStreamTransportRequest{
		SessionCommandType: 0, // Start (per HAP spec)
		TransportType:      0, // HDS
		ControllerKeySalt:  controllerSalt,
	}

	// Write request to characteristic
	if err := char.Write(&req); err != nil {
		return fmt.Errorf("failed to write HDS transport request: %w", err)
	}

	log.Printf("[homekit] HDS: wrote request to char, value=%v", char.Value)
	if err := p.putCharacteristicWithResponse(char); err != nil {
		return fmt.Errorf("failed to PUT HDS transport characteristic: %w", err)
	}

	var res camera.SetupDataStreamTransportResponse
	if err := char.ReadTLV8(&res); err != nil {
		return fmt.Errorf("failed to decode HDS transport response: %w", err)
	}

	if res.Status != 0 {
		return fmt.Errorf("HDS transport setup failed with status %d", res.Status)
	}

	// Connect to HDS port
	host := p.client.hap.DeviceHost()
	port := res.TransportTypeSessionParameters.TCPListeningPort
	addr := fmt.Sprintf("%s:%d", host, port)

	log.Printf("[homekit] HDS: connecting to %s", addr)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect to HDS port: %w", err)
	}

	// Create secured HDS connection (as controller)
	salt := controllerSalt + res.AccessoryKeySalt
	hapConn := p.client.hap.Conn.(*hap.Conn)
	p.hdsConn, err = hds.NewConn(conn, hapConn.SharedKey, salt, true)
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to create HDS connection: %w", err)
	}

	log.Printf("[homekit] HDS: transport established")
	return nil
}

func (p *HDSProducer) putCharacteristicWithResponse(char *hap.Character) error {
	wantReply := true
	reqBody := hap.JSONCharacters{
		Value: []hap.JSONCharacter{
			{AID: hap.DeviceAID, IID: char.IID, Value: char.Value, Reply: wantReply},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal PUT request: %w", err)
	}

	res, err := p.client.hap.Put(hap.PathCharacteristics, hap.MimeJSON, bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	log.Printf("[homekit] HDS: PUT status: %s", res.Status)

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("failed to read PUT response: %w", err)
	}

	log.Printf("[homekit] HDS: PUT response body: %s", bytes.TrimSpace(resBody))

	var v hap.JSONCharacters
	if len(resBody) > 0 {
		if err := json.Unmarshal(resBody, &v); err != nil {
			return fmt.Errorf("failed to unmarshal PUT response: %w", err)
		}
	}

	if len(v.Value) > 0 && v.Value[0].Status != nil {
		var statusCode int
		switch s := v.Value[0].Status.(type) {
		case float64:
			statusCode = int(s)
		case int:
			statusCode = s
		case int64:
			statusCode = int(s)
		}
		if statusCode != 0 {
			return fmt.Errorf("camera rejected HDS setup with status %d", statusCode)
		}
	}

	if len(v.Value) > 0 && v.Value[0].Value != nil {
		char.Value = v.Value[0].Value
		return nil
	}

	// Fallback: try GET if response had no value
	query := fmt.Sprintf("%d.%d", hap.DeviceAID, char.IID)
	getRes, err := p.client.hap.Get(hap.PathCharacteristics + "?id=" + query)
	if err != nil {
		return fmt.Errorf("no value in PUT response and GET failed: %w", err)
	}
	defer getRes.Body.Close()
	log.Printf("[homekit] HDS: GET status: %s", getRes.Status)
	getBody, err := io.ReadAll(getRes.Body)
	if err != nil {
		return fmt.Errorf("failed to read GET response: %w", err)
	}
	log.Printf("[homekit] HDS: GET response body: %s", bytes.TrimSpace(getBody))
	if err := json.Unmarshal(getBody, &v); err != nil {
		return fmt.Errorf("failed to unmarshal GET response: %w", err)
	}
	if len(v.Value) == 0 {
		return fmt.Errorf("camera returned empty response")
	}
	if v.Value[0].Status != nil {
		var statusCode int
		switch s := v.Value[0].Status.(type) {
		case float64:
			statusCode = int(s)
		case int:
			statusCode = s
		case int64:
			statusCode = int(s)
		}
		if statusCode != 0 {
			return fmt.Errorf("camera rejected HDS setup with status %d", statusCode)
		}
	}
	char.Value = v.Value[0].Value
	return nil
}

// sendHello sends the initial hello message
func (p *HDSProducer) sendHello() error {
	id := p.requestID.Add(1)
	p.helloID = id
	log.Printf("[homekit] HDS: sending control.hello (id=%d)", id)
	body := map[string]any{
		"version": int64(1),
	}
	return p.hdsConn.SendRequest(hds.ProtocolControl, hds.TopicHello, id, body)
}

// requestVideoStream requests a video stream via dataSend.open
func (p *HDSProducer) requestVideoStream() error {
	id := p.requestID.Add(1)
	streamID := id

	log.Printf("[homekit] HDS: requesting video stream (id=%d, streamId=%d)", id, streamID)

	body := map[string]any{
		"streamId": streamID,
		"target":   "controller",
		"type":     "ipcamera.recording",
		"reason":   "live",
	}

	return p.hdsConn.SendRequest(hds.ProtocolDataSend, hds.TopicOpen, id, body)
}

// processMessages reads and processes HDS messages
func (p *HDSProducer) processMessages() error {
	var fragmentBuffer []byte
	var initBuffer []byte
	var currentSeq int64 = -1
	var initSeq int64 = -1
	var streamRequested bool

	deadline := time.NewTimer(core.ConnDeadline)

	for {
		select {
		case <-deadline.C:
			return fmt.Errorf("connection timeout")
		default:
		}

		// Set read deadline
		if err := p.hdsConn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return err
		}

		msg, err := p.hdsConn.ReadMessage()
		if err != nil {
			return fmt.Errorf("failed to read HDS message: %w", err)
		}
		log.Printf("[homekit] HDS: msg type=%d protocol=%s topic=%s id=%d status=%d", msg.Type, msg.Protocol, msg.Topic, msg.ID, msg.Status)

		// Handle different message types
		switch {
		case msg.IsResponse() && msg.Protocol == hds.ProtocolControl && msg.Topic == hds.TopicHello:
			log.Printf("[homekit] HDS: received control.hello response (id=%d, status=%d)", msg.ID, msg.Status)
			if msg.ID == p.helloID && !streamRequested {
				if err := p.requestVideoStream(); err != nil {
					return fmt.Errorf("failed to request video stream: %w", err)
				}
				streamRequested = true
			}

		case msg.IsResponse() && msg.Protocol == hds.ProtocolDataSend && msg.Topic == hds.TopicOpen:
			if msg.Status != 0 {
				log.Printf("[homekit] HDS: dataSend.open error body=%v", msg.Body)
				return fmt.Errorf("HDS dataSend.open failed with status %d", msg.Status)
			}
			streamID, _ := msg.Body["streamId"].(int64)
			log.Printf("[homekit] HDS: stream opened (streamId=%d)", streamID)

		case msg.IsEvent() && msg.Protocol == hds.ProtocolDataSend && msg.Topic == hds.TopicData:
			deadline.Reset(core.ConnDeadline)

			// Extract video packets
			packets, ok := msg.Body["packets"].([]any)
			if !ok {
				if !p.loggedData {
					log.Printf("[homekit] HDS: data event without packets, body keys=%v", mapKeys(msg.Body))
					p.loggedData = true
				}
				continue
			}
			if !p.loggedData {
				log.Printf("[homekit] HDS: data event packets=%d", len(packets))
			}

			for _, pkt := range packets {
				packet, ok := pkt.(map[string]any)
				if !ok {
					continue
				}

				data, ok := packet["data"].([]byte)
				if !ok {
					if !p.loggedData {
						log.Printf("[homekit] HDS: packet data type=%T", packet["data"])
					}
					continue
				}

				metadata, ok := packet["metadata"].(map[string]any)
				if !ok {
					if !p.loggedData {
						log.Printf("[homekit] HDS: packet metadata type=%T", packet["metadata"])
					}
					continue
				}

				meta := hds.ParseDataSendMetadata(metadata)
				if !p.loggedData {
					log.Printf("[homekit] HDS: first metadata keys=%v", mapKeys(metadata))
					log.Printf("[homekit] HDS: first metadata=%v", metadata)
					log.Printf("[homekit] HDS: first meta type=%d seq=%d chunk=%d last=%v total=%d data=%d",
						meta.DataType, meta.DataSequenceNumber, meta.DataChunkSequenceNumber,
						meta.IsLastDataChunk, meta.DataTotalSize, len(data),
					)
					p.loggedData = true
				}

				// Handle initialization segment
				if meta.DataType == hds.DataTypeMediaInit {
					if meta.DataSequenceNumber != initSeq {
						initSeq = meta.DataSequenceNumber
						capacity := int(meta.DataTotalSize)
						if capacity == 0 {
							capacity = len(data)
						}
						initBuffer = make([]byte, 0, capacity)
						log.Printf("[homekit] HDS: init seq=%d total=%d bytes", initSeq, meta.DataTotalSize)
					}

					initBuffer = append(initBuffer, data...)

					if meta.IsLastDataChunk {
						log.Printf("[homekit] HDS: received init segment (%d bytes)", len(initBuffer))
						if err := p.demuxer.SetInit(initBuffer); err != nil {
							return fmt.Errorf("failed to set init segment: %w", err)
						}
						log.Printf("[homekit] HDS: codec=%s, SPS=%d bytes, PPS=%d bytes",
							p.demuxer.VideoCodec, len(p.demuxer.SPS), len(p.demuxer.PPS))
						if p.videoTrack != nil && len(p.demuxer.SPS) > 0 && len(p.demuxer.PPS) > 0 {
							avcc := make([]byte, 0, len(p.demuxer.SPS)+len(p.demuxer.PPS)+8)
							avcc = append(avcc, 0, 0, 0, 0)
							binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(p.demuxer.SPS)))
							avcc = append(avcc, p.demuxer.SPS...)
							avcc = append(avcc, 0, 0, 0, 0)
							binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(p.demuxer.PPS)))
							avcc = append(avcc, p.demuxer.PPS...)
							p.videoTrack.Codec = h264.AVCCToCodec(avcc)
							if !strings.Contains(p.videoTrack.Codec.FmtpLine, "sprop-parameter-sets=") {
								p.videoTrack.Codec.FmtpLine = h264.GetFmtpLine(avcc)
							}
						}
						initBuffer = nil
					}
					continue
				}

				// Handle media fragments
				if meta.DataType == hds.DataTypeMediaFragment {
					// Start of new fragment
					if meta.DataSequenceNumber != currentSeq {
						currentSeq = meta.DataSequenceNumber
						capacity := int(meta.DataTotalSize)
						if capacity == 0 {
							capacity = len(data)
						}
						fragmentBuffer = make([]byte, 0, capacity)
						log.Printf("[homekit] HDS: fragment seq=%d, total=%d bytes",
							currentSeq, meta.DataTotalSize)
					}

					// Accumulate fragment data
					fragmentBuffer = append(fragmentBuffer, data...)

					// Process complete fragment
					if meta.IsLastDataChunk {
						log.Printf("[homekit] HDS: demuxing fragment seq=%d (%d bytes)",
							currentSeq, len(fragmentBuffer))
						if err := p.demuxer.Demux(fragmentBuffer); err != nil {
							log.Printf("[homekit] HDS: demux error: %v", err)
							continue
						}
						fragmentBuffer = nil
					}
				}
			}

		case msg.IsEvent() && msg.Protocol == hds.ProtocolDataSend && msg.Topic == hds.TopicClose:
			log.Printf("[homekit] HDS: stream closed by accessory")
			return fmt.Errorf("stream closed by accessory")
		}
	}
}

// handleVideoFrame is called by the demuxer for each decoded frame
func (p *HDSProducer) handleVideoFrame(nalus [][]byte, keyframe bool, pts, dts uint64) {
	// Combine NALUs with the same PTS into one access unit before sending.
	if len(p.pendingPayload) > 0 && pts != p.pendingPTS {
		p.flushPending()
	}
	if len(p.pendingPayload) == 0 {
		p.pendingPTS = pts
		p.pendingKey = keyframe
		p.pendingHasSPS = false
		p.pendingHasPPS = false
	} else if keyframe {
		p.pendingKey = true
	}

	for _, nalu := range nalus {
		if len(nalu) == 0 {
			continue
		}
		switch nalu[0] & 0x1F {
		case h264.NALUTypeSPS:
			if p.pendingHasSPS {
				continue
			}
			p.pendingHasSPS = true
		case h264.NALUTypePPS:
			if p.pendingHasPPS {
				continue
			}
			p.pendingHasPPS = true
		}

		// AVCC format: 4-byte length + NALU
		p.pendingPayload = append(p.pendingPayload, byte(len(nalu)>>24), byte(len(nalu)>>16), byte(len(nalu)>>8), byte(len(nalu)))
		p.pendingPayload = append(p.pendingPayload, nalu...)
	}
}

func (p *HDSProducer) flushPending() {
	if len(p.pendingPayload) == 0 {
		return
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{Timestamp: uint32(p.pendingPTS)},
		Payload: p.pendingPayload,
	}

	if p.pendingKey {
		log.Printf("[homekit] HDS: keyframe pts=%d, size=%d bytes", p.pendingPTS, len(p.pendingPayload))
		if !p.loggedKeyframe && !h264.IsKeyframe(p.pendingPayload) {
			log.Printf("[homekit] HDS: keyframe payload missing IDR")
			p.loggedKeyframe = true
		}
	}

	p.videoTrack.WriteRTP(pkt)
	p.client.Recv += len(pkt.Payload)

	p.pendingPayload = nil
	p.pendingKey = false
	p.pendingHasSPS = false
	p.pendingHasPPS = false
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
