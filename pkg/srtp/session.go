package srtp

import (
	"log"
	"net"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

type Session struct {
	Local  *Endpoint
	Remote *Endpoint

	OnReadRTP func(packet *rtp.Packet)

	Recv int // bytes recv
	Send int // bytes send

	conn net.PacketConn // local conn endpoint

	PayloadType  uint8
	RTCPInterval time.Duration

	CryptoSuite byte

	senderRTCP rtcp.SenderReport
	senderTime time.Time
}

func (s *Session) Init() error {
	return s.init()
}

func (s *Session) SetConn(conn net.PacketConn) {
	s.conn = conn
}

func (s *Session) Conn() net.PacketConn {
	return s.conn
}

type Endpoint struct {
	Addr       string
	Port       uint16
	MasterKey  []byte
	MasterSalt []byte
	SSRC       uint32

	addr net.Addr
	srtp *srtp.Context
}

func (e *Endpoint) init() (err error) {
	e.addr = &net.UDPAddr{IP: net.ParseIP(e.Addr), Port: int(e.Port)}
	if len(e.MasterKey) == 0 && len(e.MasterSalt) == 0 {
		e.srtp = nil
		return nil
	}
	e.srtp, err = srtp.CreateContext(e.MasterKey, e.MasterSalt, profile(e.MasterKey))
	return
}

func profile(key []byte) srtp.ProtectionProfile {
	switch len(key) {
	case 16:
		return srtp.ProtectionProfileAes128CmHmacSha1_80
		//case 32:
		//	return srtp.ProtectionProfileAes256CmHmacSha1_80
	}
	return 0
}

func (s *Session) init() error {
	if err := s.Local.init(); err != nil {
		return err
	}
	if err := s.Remote.init(); err != nil {
		return err
	}

	s.senderRTCP.SSRC = s.Local.SSRC
	s.senderTime = time.Now().Add(s.RTCPInterval)

	return nil
}

func (s *Session) ServeConn(conn net.PacketConn) {
	if conn != nil {
		s.conn = conn
	}
	go s.handleConn()
}

func (s *Session) handleConn() {
	if s.conn == nil {
		return
	}
	b := make([]byte, 2048)
	seenAny := false
	for {
		n, addr, err := s.conn.ReadFrom(b)
		if err != nil {
			return
		}

		packetType := b[1]
		if !seenAny {
			seenAny = true
			log.Printf("[srtp] recv packet type=%d from=%v size=%d", packetType, addr, n)
		}

		switch packetType {
		case 99, 110, 0x80 | 99, 0x80 | 110:
			s.ReadRTP(b[:n])
		case 200, 201, 202, 203, 204, 205, 206, 207:
			s.ReadRTCP(b[:n])
		}
	}
}

func (s *Session) WriteRTP(packet *rtp.Packet) (int, error) {
	if s.Local.srtp == nil {
		if s.Local.addr == nil {
			return 0, nil // before init call
		}
		b, err := packet.Marshal()
		if err != nil {
			return 0, err
		}
		return s.conn.WriteTo(b, s.Remote.addr)
	}

	if now := time.Now(); now.After(s.senderTime) {
		s.senderRTCP.NTPTime = uint64(now.UnixNano())
		s.senderTime = now.Add(s.RTCPInterval)
		_, _ = s.WriteRTCP(&s.senderRTCP)
	}

	clone := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         packet.Marker,
			PayloadType:    s.PayloadType,
			SequenceNumber: packet.SequenceNumber,
			Timestamp:      packet.Timestamp,
			SSRC:           s.Local.SSRC,
		},
		Payload: packet.Payload,
	}

	b, err := clone.Marshal()
	if err != nil {
		return 0, err
	}

	s.senderRTCP.PacketCount++
	s.senderRTCP.RTPTime = clone.Timestamp
	s.senderRTCP.OctetCount += uint32(len(clone.Payload))

	if b, err = s.Local.srtp.EncryptRTP(nil, b, nil); err != nil {
		return 0, err
	}

	return s.conn.WriteTo(b, s.Remote.addr)
}

func (s *Session) WriteRTCP(packet rtcp.Packet) (int, error) {
	b, err := packet.Marshal()
	if err != nil {
		return 0, err
	}
	if s.Local.srtp != nil {
		b, err = s.Local.srtp.EncryptRTCP(nil, b, nil)
		if err != nil {
			return 0, err
		}
	}
	return s.conn.WriteTo(b, s.Remote.addr)
}

func (s *Session) ReadRTP(b []byte) {
	packet := &rtp.Packet{}

	if s.Remote.srtp != nil {
		var err error
		b, err = s.Remote.srtp.DecryptRTP(nil, b, &packet.Header)
		if err != nil {
			return
		}
	}

	if err := packet.Unmarshal(b); err != nil {
		return
	}

	if s.OnReadRTP != nil {
		s.OnReadRTP(packet)
	}
}

func (s *Session) ReadRTCP(b []byte) {
	header := rtcp.Header{}
	if s.Remote.srtp != nil {
		var err error
		b, err = s.Remote.srtp.DecryptRTCP(nil, b, &header)
		if err != nil {
			return
		}
	} else if err := header.Unmarshal(b); err != nil {
		return
	}

	//packets, err := rtcp.Unmarshal(b)
	//if err != nil {
	//	return
	//}
	//if report, ok := packets[0].(*rtcp.SenderReport); ok {
	//	log.Printf("[srtp] rtcp type=%d report=%v", header.Type, report)
	//}

	if header.Type != rtcp.TypeSenderReport {
		return
	}

	receiverRTCP := rtcp.ReceiverReport{SSRC: s.Local.SSRC}
	_, _ = s.WriteRTCP(&receiverRTCP)
}
