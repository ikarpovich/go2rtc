package homekit

import (
	"fmt"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
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
}

// startHDS initiates HDS streaming mode
func (c *Client) startHDS() error {
	log.Printf("[homekit] using HDS producer mode")

	acc, err := c.hap.GetFirstAccessory()
	if err != nil {
		return fmt.Errorf("failed to get accessory: %w", err)
	}

	// Check for HDS support
	char := acc.GetCharacter(camera.TypeSupportedDataStreamTransportConfiguration)
	if char == nil {
		return fmt.Errorf("accessory does not support HDS")
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

	// Setup fMP4 demuxer
	producer.demuxer = fmp4.NewDemuxer()
	producer.demuxer.SetOnFrame(producer.handleVideoFrame)

	// Setup HDS transport
	if err := producer.setupHDSTransport(); err != nil {
		return fmt.Errorf("failed to setup HDS transport: %w", err)
	}

	// Send hello
	if err := producer.sendHello(); err != nil {
		return fmt.Errorf("failed to send hello: %w", err)
	}

	// Request video stream
	if err := producer.requestVideoStream(); err != nil {
		return fmt.Errorf("failed to request video stream: %w", err)
	}

	// Process HDS messages
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

	// Generate controller key salt
	controllerSalt := core.RandString(32, 0)

	// Create request
	req := camera.SetupDataStreamTransportRequest{
		SessionCommandType: 1, // Start
		TransportType:      0, // HDS
		ControllerKeySalt:  controllerSalt,
	}

	// Write request to characteristic
	if err := char.Write(&req); err != nil {
		return fmt.Errorf("failed to write HDS transport request: %w", err)
	}

	log.Printf("[homekit] HDS: wrote request to char, value=%v", char.Value)

	// Send PUT request via HTTP
	if err := p.client.hap.PutCharacters(char); err != nil {
		return fmt.Errorf("failed to PUT HDS transport characteristic: %w", err)
	}

	log.Printf("[homekit] HDS: sent PUT request")

	// Read response
	if err := p.client.hap.GetCharacter(char); err != nil {
		return fmt.Errorf("failed to GET HDS transport response: %w", err)
	}

	log.Printf("[homekit] HDS: got response, char.Value type=%T, value=%v", char.Value, char.Value)

	var res camera.SetupDataStreamTransportResponse
	if err := char.ReadTLV8(&res); err != nil {
		return fmt.Errorf("failed to read HDS transport response: %w", err)
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

// sendHello sends the initial hello message
func (p *HDSProducer) sendHello() error {
	log.Printf("[homekit] HDS: sending control.hello")
	body := map[string]any{
		"version": int64(1),
	}
	return p.hdsConn.SendEvent(hds.ProtocolControl, hds.TopicHello, body)
}

// requestVideoStream requests a video stream via dataSend.open
func (p *HDSProducer) requestVideoStream() error {
	id := p.requestID.Add(1)

	log.Printf("[homekit] HDS: requesting video stream (id=%d)", id)

	body := map[string]any{
		"target": "controller",
		"type":   "camera.recording",
	}

	return p.hdsConn.SendRequest(hds.ProtocolDataSend, hds.TopicOpen, id, body)
}

// processMessages reads and processes HDS messages
func (p *HDSProducer) processMessages() error {
	var fragmentBuffer []byte
	var currentSeq int64 = -1

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

		// Handle different message types
		switch {
		case msg.IsResponse() && msg.Protocol == hds.ProtocolDataSend && msg.Topic == hds.TopicOpen:
			// Stream opened successfully
			streamID, _ := msg.Body["streamId"].(int64)
			log.Printf("[homekit] HDS: stream opened (streamId=%d)", streamID)

		case msg.IsEvent() && msg.Protocol == hds.ProtocolDataSend && msg.Topic == hds.TopicData:
			deadline.Reset(core.ConnDeadline)

			// Extract video packets
			packets, ok := msg.Body["packets"].([]any)
			if !ok {
				continue
			}

			for _, pkt := range packets {
				packet, ok := pkt.(map[string]any)
				if !ok {
					continue
				}

				data, ok := packet["data"].([]byte)
				if !ok {
					continue
				}

				metadata, ok := packet["metadata"].(map[string]any)
				if !ok {
					continue
				}

				meta := hds.ParseDataSendMetadata(metadata)

				// Handle initialization segment
				if meta.DataType == hds.DataTypeMediaInit {
					if meta.IsLastDataChunk {
						log.Printf("[homekit] HDS: received init segment (%d bytes)", len(data))
						if err := p.demuxer.SetInit(data); err != nil {
							return fmt.Errorf("failed to set init segment: %w", err)
						}
						log.Printf("[homekit] HDS: codec=%s, SPS=%d bytes, PPS=%d bytes",
							p.demuxer.VideoCodec, len(p.demuxer.SPS), len(p.demuxer.PPS))
					}
					continue
				}

				// Handle media fragments
				if meta.DataType == hds.DataTypeMediaFragment {
					// Start of new fragment
					if meta.DataSequenceNumber != currentSeq {
						currentSeq = meta.DataSequenceNumber
						fragmentBuffer = make([]byte, 0, int(meta.DataTotalSize))
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
	// Convert NALUs to AVCC format and send as single RTP packet
	// This matches the pattern used in pkg/magic/bitstream/producer.go
	payload := make([]byte, 0, 64*1024)
	for _, nalu := range nalus {
		// AVCC format: 4-byte length + NALU
		payload = append(payload, byte(len(nalu)>>24), byte(len(nalu)>>16), byte(len(nalu)>>8), byte(len(nalu)))
		payload = append(payload, nalu...)
	}

	pkt := &rtp.Packet{
		Header:  rtp.Header{Timestamp: uint32(pts)},
		Payload: payload,
	}

	if keyframe {
		log.Printf("[homekit] HDS: keyframe nalus=%d, pts=%d, size=%d bytes", len(nalus), pts, len(payload))
	}

	p.videoTrack.WriteRTP(pkt)
	p.client.Recv += len(pkt.Payload)
}
