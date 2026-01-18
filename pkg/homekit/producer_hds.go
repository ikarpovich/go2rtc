package homekit

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/hap"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
	"github.com/AlexxIT/go2rtc/pkg/hap/hds"
	"github.com/pion/rtp"
)

// HDSProducer handles video streaming from HDS (HomeKit Data Stream) cameras
type HDSProducer struct {
	client *Client

	hdsConn *hds.Conn
	demuxer hdsDemuxer

	videoTrack *core.Receiver

	requestID atomic.Int64
	helloID   int64

	loggedData bool

	pendingPayload []byte
	pendingPTS     uint64
	pendingDTS     uint64
	pendingKey     bool
	pendingHasSPS  bool
	pendingHasPPS  bool
	loggedKeyframe bool
	loggedTypes    bool
	loggedPrefix   bool

	lastPTS         uint64
	lastInputDTS    uint64
	lastOutputDTS   uint64
	frameDur        uint64
	captureDir      string
	captureSeconds  uint64
	captureWindows  int
	captureIndex    int
	captureStartPTS uint64
	captureBuf      []byte
	captureInitDone bool
}

// startHDS initiates HDS streaming mode
func (c *Client) startHDS() error {
	log.Printf("[homekit] using HDS producer mode")

	acc, err := c.hap.GetFirstAccessory()
	if err != nil {
		return fmt.Errorf("failed to get accessory: %w", err)
	}

	if err := c.applyRecordingConfig(acc); err != nil {
		log.Printf("[homekit] HDS: recording config not applied: %v", err)
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

	producer.captureDir = "/tmp/go2rtc-hds-dumps"
	producer.captureSeconds = 10
	producer.captureWindows = 3
	if err := os.MkdirAll(producer.captureDir, 0o755); err == nil {
		log.Printf("[homekit] HDS: dumping raw fragments to %s", producer.captureDir)
	}

	// Get video track
	producer.videoTrack = c.trackByKind(core.KindVideo)
	if producer.videoTrack == nil {
		return fmt.Errorf("no video track configured")
	}

	log.Printf("[homekit] HDS: video track codec=%s", producer.videoTrack.Codec.Name)
	producer.videoTrack.Codec.PayloadType = core.PayloadTypeRAW

	for {
		producer.resetForReconnect()

		// Setup HDS transport
		if err := producer.setupHDSTransport(); err != nil {
			log.Printf("[homekit] HDS: setup transport failed: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Send hello and wait for the response before requesting a stream.
		if err := producer.sendHello(); err != nil {
			log.Printf("[homekit] HDS: send hello failed: %v", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Process HDS messages (will request stream after hello)
		if err := producer.processMessages(); err != nil {
			log.Printf("[homekit] HDS: stream ended (%v), reconnecting", err)
			if producer.hdsConn != nil {
				_ = producer.hdsConn.Close()
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
	}
}

func (p *HDSProducer) resetForReconnect() {
	p.demuxer = newHDSMP4FFDemuxer()
	p.demuxer.SetOnFrame(p.handleVideoFrame)
	p.pendingPayload = nil
	p.pendingPTS = 0
	p.pendingDTS = 0
	p.pendingKey = false
	p.pendingHasSPS = false
	p.pendingHasPPS = false
	p.lastInputDTS = 0
	p.lastOutputDTS = 0
	p.lastPTS = 0
	p.frameDur = 0
	p.loggedData = false
	p.loggedTypes = false
	p.loggedKeyframe = false
	p.loggedPrefix = false
	p.captureInitDone = false
	p.captureStartPTS = 0
	p.captureIndex = 0
	p.captureBuf = nil
}

func (c *Client) applyRecordingConfig(acc *hap.Accessory) error {
	selectedChar := acc.GetCharacter(camera.TypeSelectedCameraRecordingConfiguration)
	if selectedChar != nil {
		var selected camera.SelectedCameraRecordingConfiguration
		if err := selectedChar.ReadTLV8(&selected); err == nil {
			if len(selected.VideoConfig.CodecConfigs) > 0 {
				log.Printf("[homekit] HDS: selected recording config already set (width=%d height=%d fps=%d)",
					selected.VideoConfig.CodecConfigs[0].CodecAttrs.Width,
					selected.VideoConfig.CodecConfigs[0].CodecAttrs.Height,
					selected.VideoConfig.CodecConfigs[0].CodecAttrs.Framerate,
				)
				return nil
			}
		}
	}

	char := acc.GetCharacter(camera.TypeSupportedCameraRecordingConfiguration)
	if char == nil {
		return nil
	}
	var general camera.SupportedCameraRecordingConfiguration
	if err := char.ReadTLV8(&general); err != nil {
		return fmt.Errorf("read supported camera recording config: %w", err)
	}

	char = acc.GetCharacter(camera.TypeSupportedVideoRecordingConfiguration)
	if char == nil {
		return nil
	}
	var video camera.SupportedVideoRecordingConfiguration
	if err := char.ReadTLV8(&video); err != nil {
		return fmt.Errorf("read supported video recording config: %w", err)
	}
	if len(video.CodecConfigs) == 0 {
		return nil
	}
	for i, cfg := range video.CodecConfigs {
		log.Printf("[homekit] HDS: supported recording video[%d] width=%d height=%d fps=%d bitrate=%d",
			i, cfg.CodecAttrs.Width, cfg.CodecAttrs.Height, cfg.CodecAttrs.Framerate, cfg.CodecParams.Bitrate)
	}
	bestVideo := video.CodecConfigs[0]
	bestArea := uint32(bestVideo.CodecAttrs.Width) * uint32(bestVideo.CodecAttrs.Height)
	bestFPS := bestVideo.CodecAttrs.Framerate
	for _, cfg := range video.CodecConfigs[1:] {
		area := uint32(cfg.CodecAttrs.Width) * uint32(cfg.CodecAttrs.Height)
		if area > bestArea || (area == bestArea && cfg.CodecAttrs.Framerate > bestFPS) {
			bestArea = area
			bestFPS = cfg.CodecAttrs.Framerate
			bestVideo = cfg
		}
	}
	log.Printf("[homekit] HDS: selected recording video width=%d height=%d fps=%d",
		bestVideo.CodecAttrs.Width, bestVideo.CodecAttrs.Height, bestVideo.CodecAttrs.Framerate)

	var audio camera.SupportedAudioRecordingConfiguration
	var bestAudio camera.AudioRecordingCodecConfiguration
	hasAudio := false
	char = acc.GetCharacter(camera.TypeSupportedAudioRecordingConfiguration)
	if char != nil {
		if err := char.ReadTLV8(&audio); err == nil && len(audio.CodecConfigs) > 0 {
			bestAudio = audio.CodecConfigs[0]
			hasAudio = true
		}
	}
	selected := camera.SelectedCameraRecordingConfiguration{
		GeneralConfig: general,
		VideoConfig: camera.SupportedVideoRecordingConfiguration{
			CodecConfigs: []camera.VideoRecordingCodecConfiguration{bestVideo},
		},
	}
	if hasAudio {
		selected.AudioConfig = camera.SupportedAudioRecordingConfiguration{
			CodecConfigs: []camera.AudioRecordingCodecConfiguration{bestAudio},
		}
	}

	char = acc.GetCharacter(camera.TypeSelectedCameraRecordingConfiguration)
	if char == nil {
		return nil
	}
	if err := char.Write(selected); err != nil {
		return fmt.Errorf("write selected camera recording config: %w", err)
	}
	if err := c.hap.PutCharacters(char); err != nil {
		return fmt.Errorf("put selected camera recording config: %w", err)
	}
	log.Printf("[homekit] HDS: selected recording config applied")
	return nil
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
						sps := p.demuxer.SPS()
						pps := p.demuxer.PPS()
						log.Printf("[homekit] HDS: SPS=%d bytes, PPS=%d bytes", len(sps), len(pps))
						if p.videoTrack != nil && len(sps) > 0 && len(pps) > 0 {
							sps = stripStartCode(sps)
							pps = stripStartCode(pps)
							avcc := make([]byte, 0, len(sps)+len(pps)+8)
							avcc = append(avcc, 0, 0, 0, 0)
							binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(sps)))
							avcc = append(avcc, sps...)
							avcc = append(avcc, 0, 0, 0, 0)
							binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(pps)))
							avcc = append(avcc, pps...)
							updateCodecFromAVCC(p.videoTrack.Codec, avcc)
							if !strings.Contains(p.videoTrack.Codec.FmtpLine, "sprop-parameter-sets=") {
								p.videoTrack.Codec.FmtpLine = h264.GetFmtpLine(avcc)
							}
							log.Printf("[homekit] HDS: fmtp=%s", p.videoTrack.Codec.FmtpLine)
						}
						p.dumpInit(initBuffer)
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
						p.captureFragment(fragmentBuffer)
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
	p.lastPTS = pts
	if len(nalus) > 0 && p.videoTrack != nil {
		updatedSPS := false
		updatedPPS := false
		for _, nalu := range nalus {
			if len(nalu) == 0 {
				continue
			}
			switch nalu[0] & 0x1F {
			case h264.NALUTypeSPS:
				sps := stripStartCode(nalu)
				if len(sps) > 0 && !bytes.Equal(sps, p.demuxer.SPS()) {
					p.demuxer.SetSPS(sps)
					updatedSPS = true
					if info := h264.DecodeSPS(sps); info != nil {
						log.Printf("[homekit] HDS: SPS updated width=%d height=%d", info.Width(), info.Height())
					}
				}
			case h264.NALUTypePPS:
				pps := stripStartCode(nalu)
				if len(pps) > 0 && !bytes.Equal(pps, p.demuxer.PPS()) {
					p.demuxer.SetPPS(pps)
					updatedPPS = true
				}
			}
		}
		if len(p.demuxer.SPS()) > 0 && len(p.demuxer.PPS()) > 0 &&
			(updatedSPS || updatedPPS || !strings.Contains(p.videoTrack.Codec.FmtpLine, "sprop-parameter-sets=")) {
			sps := p.demuxer.SPS()
			pps := p.demuxer.PPS()
			avcc := make([]byte, 0, len(sps)+len(pps)+8)
			avcc = append(avcc, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(sps)))
			avcc = append(avcc, sps...)
			avcc = append(avcc, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(pps)))
			avcc = append(avcc, pps...)
			updateCodecFromAVCC(p.videoTrack.Codec, avcc)
			if !strings.Contains(p.videoTrack.Codec.FmtpLine, "sprop-parameter-sets=") {
				p.videoTrack.Codec.FmtpLine = h264.GetFmtpLine(avcc)
			}
			log.Printf("[homekit] HDS: fmtp (inband)=%s", p.videoTrack.Codec.FmtpLine)
		}
	}
	// Combine NALUs with the same PTS into one access unit before sending.
	if len(p.pendingPayload) > 0 && pts != p.pendingPTS {
		p.flushPending()
	}
	isKey := false
	if !p.loggedTypes {
		var (
			types  []byte
			prefix []byte
		)
		for _, nalu := range nalus {
			if len(nalu) == 0 {
				continue
			}
			types = append(types, nalu[0]&0x1F)
			if len(prefix) == 0 {
				if len(nalu) > 8 {
					prefix = nalu[:8]
				} else {
					prefix = nalu
				}
			}
		}
		log.Printf("[homekit] HDS: nalu count=%d types=%v prefix=%x sps=%d pps=%d", len(nalus), types, prefix, len(p.demuxer.SPS()), len(p.demuxer.PPS()))
		p.loggedTypes = true
	}
	for _, nalu := range nalus {
		if len(nalu) > 0 && (nalu[0]&0x1F) == h264.NALUTypeIFrame {
			isKey = true
			break
		}
	}

	if isKey && p.demuxer != nil {
		head := make([][]byte, 0, 2+len(nalus))
		if len(p.demuxer.SPS()) > 0 {
			head = append(head, p.demuxer.SPS())
		}
		if len(p.demuxer.PPS()) > 0 {
			head = append(head, p.demuxer.PPS())
		}
		nalus = append(head, nalus...)
	}

	if len(p.pendingPayload) == 0 {
		p.pendingPTS = pts
		p.pendingDTS = dts
		p.pendingKey = isKey
		p.pendingHasSPS = false
		p.pendingHasPPS = false
	} else if isKey {
		p.pendingKey = true
	}
	if pts == p.pendingPTS && dts < p.pendingDTS {
		p.pendingDTS = dts
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

	inDTS := p.pendingPTS
	if p.pendingDTS != 0 {
		inDTS = p.pendingDTS
	}
	inPTS := p.pendingPTS
	if inPTS < inDTS {
		inPTS = inDTS
	}
	if p.frameDur == 0 {
		p.frameDur = 90000 / 30
		if p.frameDur == 0 {
			p.frameDur = 3000
		}
	}
	var outDTS uint64
	if p.lastInputDTS == 0 {
		outDTS = inDTS
	} else {
		var delta int64 = int64(inDTS) - int64(p.lastInputDTS)
		if delta <= 0 {
			delta = int64(p.frameDur)
		}
		outDTS = p.lastOutputDTS + uint64(delta)
	}
	outPTS := outDTS + (inPTS - inDTS)
	pkt := &rtp.Packet{
		Header:  rtp.Header{Timestamp: uint32(outDTS), ExtensionProfile: hdsCTS(outPTS, outDTS)},
		Payload: p.pendingPayload,
	}

	if p.pendingKey {
		log.Printf("[homekit] HDS: keyframe pts=%d, size=%d bytes", p.pendingPTS, len(p.pendingPayload))
		if !p.loggedKeyframe && !h264.IsKeyframe(p.pendingPayload) {
			log.Printf("[homekit] HDS: keyframe payload missing IDR")
			p.loggedKeyframe = true
		}
	}
	if !p.loggedPrefix {
		prefix := p.pendingPayload
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		log.Printf("[homekit] HDS: payload prefix=%x", prefix)
		p.loggedPrefix = true
	}

	p.videoTrack.WriteRTP(pkt)
	p.client.Recv += len(pkt.Payload)
	p.lastInputDTS = inDTS
	p.lastOutputDTS = outDTS

	p.pendingPayload = nil
	p.pendingKey = false
	p.pendingHasSPS = false
	p.pendingHasPPS = false
}

func stripStartCode(nalu []byte) []byte {
	if len(nalu) >= 4 && nalu[0] == 0 && nalu[1] == 0 && nalu[2] == 0 && nalu[3] == 1 {
		return nalu[4:]
	}
	if len(nalu) >= 3 && nalu[0] == 0 && nalu[1] == 0 && nalu[2] == 1 {
		return nalu[3:]
	}
	return nalu
}

func updateCodecFromAVCC(codec *core.Codec, avcc []byte) {
	if codec == nil {
		return
	}
	next := h264.AVCCToCodec(avcc)
	codec.Name = next.Name
	codec.ClockRate = next.ClockRate
	codec.PayloadType = next.PayloadType
	codec.FmtpLine = next.FmtpLine
}

func hdsCTS(pts, dts uint64) uint16 {
	if pts <= dts {
		return 0
	}
	diff := pts - dts
	if diff > 0xffff {
		return 0
	}
	return uint16(diff)
}

func (p *HDSProducer) dumpInit(data []byte) {
	if p.captureInitDone || p.captureDir == "" {
		return
	}
	path := filepath.Join(p.captureDir, "init.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		log.Printf("[homekit] HDS: dump init error: %v", err)
		return
	}
	p.captureInitDone = true
	log.Printf("[homekit] HDS: dumped init to %s", path)
}

func (p *HDSProducer) captureFragment(fragment []byte) {
	if p.captureDir == "" || p.captureWindows <= 0 {
		return
	}
	if p.lastPTS == 0 {
		return
	}
	if p.captureStartPTS == 0 {
		p.captureStartPTS = p.lastPTS
		p.captureBuf = nil
	}
	p.captureBuf = append(p.captureBuf, fragment...)
	if p.lastPTS-p.captureStartPTS < p.captureSeconds*90000 {
		return
	}
	path := filepath.Join(p.captureDir, fmt.Sprintf("window-%d.bin", p.captureIndex))
	if err := os.WriteFile(path, p.captureBuf, 0o644); err != nil {
		log.Printf("[homekit] HDS: dump window error: %v", err)
		return
	}
	log.Printf("[homekit] HDS: dumped window %d to %s", p.captureIndex, path)
	p.captureIndex++
	p.captureStartPTS = 0
	p.captureBuf = nil
	if p.captureIndex >= p.captureWindows {
		p.captureWindows = 0
	}
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
