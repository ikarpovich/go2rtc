# HDS Producer Implementation Plan

## Overview

This document outlines the implementation plan for adding HDS (HomeKit Data Stream) producer support to go2rtc. This feature will enable go2rtc to decode video from HKSV (HomeKit Secure Video) cameras like the Logitech Circle View doorbell and serve it via RTSP to consumers like Frigate.

## Problem Statement

The Logitech Circle View doorbell (and similar HKSV cameras) **only supports HDS for video streaming**, not SRTP. The current go2rtc producer mode uses SRTP, which these devices ignore entirely.

### Current Behavior
- **Proxy mode works**: iPhone can view doorbell via go2rtc proxy (transparent HDS passthrough)
- **Producer mode fails**: Frigate receives 0 video packets because doorbell ignores SRTP setup

### Root Cause
The doorbell acknowledges SRTP stream setup but never sends UDP packets. It only streams video via HDS (TCP-based encrypted protocol).

## Architecture

### Current Flow (Proxy Only)
```
iPhone ◄──HDS──► go2rtc (proxy) ◄──HDS──► Doorbell
                     │
                  io.Copy
              (transparent passthrough)
```

### Proposed Flow (Proxy + Producer)
```
                    ┌─────────────────────────────────────┐
                    │           go2rtc                    │
                    │                                     │
iPhone ◄───HDS───►  │  con ◄───────────────────► acc     │ ◄───HDS───► Doorbell
                    │       │                     │       │
                    │       │    ┌────────────┐   │       │
                    │       └────│ HDS Tapper │◄──┘       │
                    │            └─────┬──────┘           │
                    └──────────────────┼──────────────────┘
                                       │
                                       ▼
                              ┌────────────────┐
                              │  MP4 Demuxer   │
                              │  H.264 → RTP   │
                              └───────┬────────┘
                                      │
                                      ▼ RTSP
                                   Frigate
```

This design allows simultaneous:
- iPhone viewing (via HDS proxy)
- Frigate recording (via demuxed RTSP)

## HDS Protocol Deep Dive

### Connection Setup

1. **HAP Characteristic Exchange** (char 131 - `SetupDataStreamTransport`)
   - Controller writes: `{SessionCommandType, TransportType, ControllerKeySalt}`
   - Accessory responds: `{Status, TCPListeningPort, AccessoryKeySalt}`

2. **TCP Connection**
   - Controller connects to accessory's HDS TCP port
   - Keys derived via HKDF-SHA512:
     ```
     salt = controllerKeySalt + accessoryKeySalt
     writeKey = HKDF-SHA512(SharedKey, salt, "HDS-Write-Encryption-Key")
     readKey = HKDF-SHA512(SharedKey, salt, "HDS-Read-Encryption-Key")
     ```

3. **Encryption**
   - ChaCha20-Poly1305 AEAD
   - 8-byte nonce (counter)
   - 16-byte auth tag

### HDS Frame Format

```
┌─────────────┬─────────────────────┬───────────────────────┬──────────────┐
│ Type (1B)   │ Payload Length (3B) │ Encrypted Payload     │ Auth Tag     │
│ = 0x01      │ Big-Endian          │ (variable)            │ (16 bytes)   │
└─────────────┴─────────────────────┴───────────────────────┴──────────────┘
```

### HDS Message Format (Decrypted Payload)

```
┌─────────────────┬──────────────────┬────────────────────┐
│ Header Len (1B) │ Header Dict      │ Message Dict       │
└─────────────────┴──────────────────┴────────────────────┘
```

Header and Message are encoded using a custom binary format (DataStreamParser).

### HDS Protocols and Topics

| Protocol      | Topics                          | Purpose                    |
|---------------|--------------------------------|----------------------------|
| `control`     | `hello`                        | Connection handshake       |
| `targetControl` | `whoami`                     | Identity exchange          |
| `dataSend`    | `open`, `data`, `ack`, `close` | Video/data streaming       |

### Message Types

| Type | Name     | Description                    |
|------|----------|--------------------------------|
| 1    | EVENT    | One-way notification           |
| 2    | REQUEST  | Expects response               |
| 3    | RESPONSE | Reply to request               |

### Video Data Format

Video is sent via `dataSend.data` events containing:
- **Fragmented MP4** (ISO BMFF):
  - `ftyp` - File type box
  - `moov` - Movie box (codec config, SPS/PPS)
  - `moof` - Movie fragment header
  - `mdat` - Media data (H.264/H.265 NAL units)

- **Metadata**:
  - `dataType`: MEDIA_INITIALIZATION or MEDIA_FRAGMENT
  - `dataSequenceNumber`: Fragment sequence
  - `dataChunkSequenceNumber`: Chunk within fragment
  - `isLastDataChunk`: End of fragment marker
  - `dataTotalSize`: Total fragment size

## Implementation Components

### Phase 1: HDS Protocol Layer

#### 1.1 DataStream Parser (`pkg/hap/hds/parser.go`)

Binary encoding/decoding for HDS messages, matching HAP-NodeJS DataStreamParser.

```go
package hds

// DataFormatTags for HDS binary protocol
const (
    TagTrue      = 0x01
    TagFalse     = 0x02
    TagTerminator = 0x03
    TagNull      = 0x04
    // ... integer ranges, strings, data, arrays, dictionaries
)

type Reader struct {
    data []byte
    pos  int
}

type Writer struct {
    buf []byte
}

func (r *Reader) Decode() (any, error)
func (w *Writer) Encode(v any) error
```

#### 1.2 HDS Message Types (`pkg/hap/hds/message.go`)

```go
package hds

type MessageType byte

const (
    MessageEvent    MessageType = 1
    MessageRequest  MessageType = 2
    MessageResponse MessageType = 3
)

type Message struct {
    Type     MessageType
    Protocol string
    Topic    string
    ID       int64  // for request/response
    Status   int64  // for response
    Body     map[string]any
}

// Protocols
const (
    ProtocolControl    = "control"
    ProtocolDataSend   = "dataSend"
)

// Topics
const (
    TopicHello = "hello"
    TopicOpen  = "open"
    TopicData  = "data"
    TopicAck   = "ack"
    TopicClose = "close"
)
```

#### 1.3 HDS Connection Enhancement (`pkg/hap/hds/hds.go`)

Extend existing `Conn` to support message-level operations:

```go
// Add to existing Conn struct
func (c *Conn) ReadMessage() (*Message, error)
func (c *Conn) WriteMessage(msg *Message) error
func (c *Conn) SendEvent(protocol, topic string, body map[string]any) error
func (c *Conn) SendRequest(protocol, topic string, body map[string]any) (*Message, error)
func (c *Conn) SendResponse(protocol, topic string, id int64, status int64, body map[string]any) error
```

### Phase 2: HDS Stream Tapper

#### 2.1 Proxy Modification (`pkg/homekit/proxy.go`)

Replace `io.Copy` with bidirectional message handler:

```go
type HDSTapper struct {
    con *hds.Conn  // controller side (iPhone)
    acc *hds.Conn  // accessory side (doorbell)

    videoCallback func(data []byte, metadata DataSendMetadata)
}

func (t *HDSTapper) Run() error {
    // Bidirectional relay with video extraction
    go t.relayAccToCon()  // doorbell → iPhone (tap video here)
    return t.relayConToAcc()  // iPhone → doorbell
}

func (t *HDSTapper) relayAccToCon() error {
    for {
        msg, err := t.acc.ReadMessage()
        if err != nil {
            return err
        }

        // Extract video data from dataSend.data events
        if msg.Protocol == ProtocolDataSend && msg.Topic == TopicData {
            t.extractVideo(msg)
        }

        // Forward to controller
        if err := t.con.WriteMessage(msg); err != nil {
            return err
        }
    }
}

func (t *HDSTapper) extractVideo(msg *Message) {
    // Extract packets from message body
    packets := msg.Body["packets"].([]any)
    for _, p := range packets {
        packet := p.(map[string]any)
        data := packet["data"].([]byte)
        metadata := parseMetadata(packet["metadata"])
        t.videoCallback(data, metadata)
    }
}
```

### Phase 3: Fragmented MP4 Demuxer

#### 3.1 fMP4 Parser (`pkg/mp4/fmp4/demuxer.go`)

```go
package fmp4

type Demuxer struct {
    // Codec configuration from moov
    VideoCodec string  // "H264" or "H265"
    SPS        []byte
    PPS        []byte
    VPS        []byte  // H.265 only

    onFrame func(nalus [][]byte, keyframe bool, pts, dts uint64)
}

func NewDemuxer() *Demuxer

// Feed initialization segment (ftyp + moov)
func (d *Demuxer) SetInit(data []byte) error

// Feed media fragment (moof + mdat)
func (d *Demuxer) Demux(data []byte) error
```

#### 3.2 Box Parsing (`pkg/mp4/fmp4/box.go`)

```go
package fmp4

type Box struct {
    Type   string
    Size   uint64
    Data   []byte
    Children []*Box
}

func ParseBox(r io.Reader) (*Box, error)
func (b *Box) Find(path string) *Box  // e.g., "moov/trak/mdia/minf/stbl/stsd/avc1"

// Specific box parsers
func ParseAVCC(data []byte) (sps, pps []byte, err error)  // H.264
func ParseHVCC(data []byte) (vps, sps, pps []byte, err error)  // H.265
func ParseTFHD(data []byte) (*TrackFragmentHeader, error)
func ParseTRUN(data []byte) (*TrackRunBox, error)
```

### Phase 4: HDS Producer Integration

#### 4.1 Producer with HDS Support (`pkg/homekit/producer.go`)

```go
func (c *Client) Start() error {
    // Check if camera supports SRTP or HDS-only
    acc, _ := c.hap.GetFirstAccessory()

    // Check for HDS support (char 130 + 131)
    if char := acc.GetCharacter(camera.TypeSupportedDataStreamTransportConfiguration); char != nil {
        return c.startHDS()
    }

    // Fall back to SRTP (existing code)
    return c.startSRTP()
}

func (c *Client) startHDS() error {
    // 1. Setup HDS session via characteristic 131
    salt, port, err := c.setupHDSTransport()
    if err != nil {
        return err
    }

    // 2. Connect to HDS TCP port
    conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", c.hap.DeviceHost(), port))
    if err != nil {
        return err
    }

    // 3. Create HDS connection (as controller)
    hdsConn, err := hds.NewConn(conn, c.hap.Conn.SharedKey, salt, true)
    if err != nil {
        return err
    }

    // 4. Send hello
    if err := c.sendHDSHello(hdsConn); err != nil {
        return err
    }

    // 5. Setup fMP4 demuxer
    demuxer := fmp4.NewDemuxer()
    demuxer.onFrame = c.handleVideoFrame

    // 6. Request video stream
    if err := c.requestVideoStream(hdsConn); err != nil {
        return err
    }

    // 7. Read and process HDS messages
    return c.processHDSStream(hdsConn, demuxer)
}

func (c *Client) handleVideoFrame(nalus [][]byte, keyframe bool, pts, dts uint64) {
    // Convert NALUs to RTP packets and send to track
    for _, nalu := range nalus {
        // Use existing H.264 packetizer
        packets := c.packetizer.Packetize(nalu, pts)
        for _, pkt := range packets {
            c.videoTrack.WriteRTP(pkt)
        }
    }
}
```

#### 4.2 Proxy + Producer Combined (`pkg/homekit/proxy_tapper.go`)

For simultaneous iPhone viewing and Frigate recording:

```go
func (p *Proxy) listenHDSWithTap(srv ServerProxy, accPort int, salt string, producer *HDSProducer) (int, error) {
    ln, err := net.ListenTCP("tcp", nil)
    if err != nil {
        return 0, err
    }

    go func() {
        defer ln.Close()

        conn1, err := ln.Accept()  // iPhone connects
        if err != nil {
            return
        }
        defer conn1.Close()

        // Secured controller conn
        con, _ := hds.NewConn(conn1, p.con.SharedKey, salt, false)

        // Connect to doorbell
        accIP := p.acc.RemoteAddr().(*net.TCPAddr).IP
        conn2, _ := net.DialTCP("tcp", nil, &net.TCPAddr{IP: accIP, Port: accPort})
        defer conn2.Close()

        // Secured accessory conn
        acc, _ := hds.NewConn(conn2, p.acc.SharedKey, salt, true)

        // Create tapper instead of io.Copy
        tapper := &HDSTapper{
            con: con,
            acc: acc,
            videoCallback: producer.HandleVideoData,
        }

        srv.AddConn(con)
        defer srv.DelConn(con)

        tapper.Run()
    }()

    return ln.Addr().(*net.TCPAddr).Port, nil
}
```

## Security Considerations

### Key Availability

All required cryptographic material is already available:

| Key | Source | Location |
|-----|--------|----------|
| HAP SharedKey | Pair-Verify exchange | `hap.Conn.SharedKey` |
| HDS Salt | Char 131 exchange | Runtime (controller + accessory salts) |
| HDS Read/Write Keys | HKDF derivation | Derived at connection time |

### No Pairing Changes Required

The existing `homekit://` URL contains all necessary credentials:
- `device_id` - Accessory identifier
- `device_public` - Accessory's Ed25519 public key
- `client_id` - Controller identifier
- `client_private` - Controller's Ed25519 private key

## File Structure

```
pkg/
├── hap/
│   └── hds/
│       ├── hds.go          # Existing: encrypted transport
│       ├── parser.go       # NEW: DataStream binary parser
│       ├── message.go      # NEW: HDS message types
│       └── protocol.go     # NEW: Protocol constants
├── mp4/
│   └── fmp4/
│       ├── demuxer.go      # NEW: fMP4 demuxer
│       └── box.go          # NEW: ISO BMFF box parsing
└── homekit/
    ├── producer.go         # MODIFY: Add HDS producer mode
    ├── proxy.go            # MODIFY: Add tapping capability
    └── tapper.go           # NEW: HDS stream tapper
```

## Testing Strategy

### Unit Tests

1. **DataStream Parser**: Encode/decode round-trip tests
2. **fMP4 Demuxer**: Parse sample fMP4 files, extract NALUs
3. **HDS Message**: Serialize/deserialize various message types

### Integration Tests

1. **Mock HDS Server**: Simulate doorbell HDS responses
2. **End-to-End**: Verify video flows from mock doorbell to RTSP output

### Manual Testing

1. Verify iPhone can still view doorbell via proxy
2. Verify Frigate receives video via RTSP
3. Verify simultaneous viewing (iPhone + Frigate)

## Implementation Order

### Milestone 1: HDS Protocol Layer
- [ ] Implement DataStream binary parser
- [ ] Implement HDS message types
- [ ] Add message-level read/write to hds.Conn
- [ ] Unit tests for parser and messages

### Milestone 2: fMP4 Demuxer
- [ ] Implement ISO BMFF box parser
- [ ] Parse ftyp/moov for codec configuration
- [ ] Parse moof/mdat for video samples
- [ ] Extract H.264/H.265 NALUs
- [ ] Unit tests with sample fMP4 data

### Milestone 3: HDS Producer Mode
- [ ] Implement HDS session setup (char 131)
- [ ] Implement HDS hello handshake
- [ ] Implement video stream request
- [ ] Integrate fMP4 demuxer
- [ ] Feed NALUs to RTP packetizer
- [ ] Test standalone producer mode

### Milestone 4: Proxy + Producer Integration
- [ ] Implement HDS stream tapper
- [ ] Replace io.Copy with tapper in proxy
- [ ] Enable simultaneous proxy + producer
- [ ] End-to-end testing

## References

- [HAP-NodeJS DataStreamServer.ts](https://github.com/homebridge/HAP-NodeJS/blob/master/src/lib/datastream/DataStreamServer.ts)
- [HAP-NodeJS DataStreamParser.ts](https://github.com/homebridge/HAP-NodeJS/blob/master/src/lib/datastream/DataStreamParser.ts)
- [HAP-NodeJS RecordingManagement.ts](https://github.com/homebridge/HAP-NodeJS/blob/master/src/lib/camera/RecordingManagement.ts)
- [ISO Base Media File Format (ISO/IEC 14496-12)](https://www.iso.org/standard/68960.html)
- [HomeKit Accessory Protocol Specification](https://developer.apple.com/homekit/)

## Appendix A: DataStream Binary Format Tags

| Tag Range | Type |
|-----------|------|
| 0x01 | True |
| 0x02 | False |
| 0x03 | Terminator |
| 0x04 | Null |
| 0x05 | UUID (16 bytes) |
| 0x06 | Date (float64 seconds since 2001-01-01) |
| 0x07 | Integer -1 |
| 0x08-0x2E | Integer 0-39 |
| 0x30 | Int8 |
| 0x31 | Int16LE |
| 0x32 | Int32LE |
| 0x33 | Int64LE |
| 0x35 | Float32LE |
| 0x36 | Float64LE |
| 0x40-0x60 | UTF8 (length 0-32 inline) |
| 0x61 | UTF8 (length8) |
| 0x62 | UTF8 (length16LE) |
| 0x63 | UTF8 (length32LE) |
| 0x64 | UTF8 (length64LE) |
| 0x6F | UTF8 (null-terminated) |
| 0x70-0x90 | Data (length 0-32 inline) |
| 0x91 | Data (length8) |
| 0x92 | Data (length16LE) |
| 0x93 | Data (length32LE) |
| 0x94 | Data (length64LE) |
| 0x9F | Data (terminated) |
| 0xA0-0xCF | Compression reference (index 0-47) |
| 0xD0-0xDE | Array (length 0-14 inline) |
| 0xDF | Array (terminated) |
| 0xE0-0xEE | Dictionary (length 0-14 inline) |
| 0xEF | Dictionary (terminated) |

## Appendix B: DataSend Message Examples

### dataSend.open Request
```json
{
  "protocol": "dataSend",
  "request": "open",
  "id": 12345,
  "message": {
    "target": "controller",
    "type": "camera.recording"
  }
}
```

### dataSend.data Event
```json
{
  "protocol": "dataSend",
  "event": "data",
  "message": {
    "streamId": 1,
    "packets": [{
      "data": "<binary>",
      "metadata": {
        "dataType": 1,
        "dataSequenceNumber": 42,
        "dataChunkSequenceNumber": 1,
        "isLastDataChunk": true,
        "dataTotalSize": 16384
      }
    }],
    "endOfStream": false
  }
}
```

### dataSend.ack Event
```json
{
  "protocol": "dataSend",
  "event": "ack",
  "message": {
    "streamId": 1,
    "endOfStream": false
  }
}
```
