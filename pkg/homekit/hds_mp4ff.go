package homekit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

type hdsDemuxer interface {
	SetOnFrame(func(nalus [][]byte, keyframe bool, pts, dts uint64))
	SetInit(data []byte) error
	Demux(data []byte) error
	SPS() []byte
	PPS() []byte
	SetSPS([]byte)
	SetPPS([]byte)
}

type hdsMP4FFDemuxer struct {
	onFrame func(nalus [][]byte, keyframe bool, pts, dts uint64)
	sps     []byte
	pps     []byte

	trackID   uint32
	timeScale uint32
	trex      *mp4.TrexBox
	baseTime  uint64
}

func newHDSMP4FFDemuxer() *hdsMP4FFDemuxer {
	return &hdsMP4FFDemuxer{}
}

func (d *hdsMP4FFDemuxer) SetOnFrame(fn func(nalus [][]byte, keyframe bool, pts, dts uint64)) {
	d.onFrame = fn
}

func (d *hdsMP4FFDemuxer) SetSPS(sps []byte) {
	d.sps = sps
}

func (d *hdsMP4FFDemuxer) SetPPS(pps []byte) {
	d.pps = pps
}

func (d *hdsMP4FFDemuxer) SPS() []byte {
	return d.sps
}

func (d *hdsMP4FFDemuxer) PPS() []byte {
	return d.pps
}

func (d *hdsMP4FFDemuxer) SetInit(data []byte) error {
	initFile, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if initFile.Init == nil || initFile.Init.Moov == nil {
		return errors.New("mp4ff: init segment missing moov")
	}

	var videoTrack *mp4.TrakBox
	for _, trak := range initFile.Init.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil || trak.Mdia.Minf.Stbl.Stsd == nil {
			continue
		}
		if trak.Mdia.Minf.Stbl.Stsd.AvcX != nil {
			videoTrack = trak
			break
		}
	}
	if videoTrack == nil {
		return errors.New("mp4ff: no AVC track in init segment")
	}

	avcC := videoTrack.Mdia.Minf.Stbl.Stsd.AvcX.AvcC
	if len(avcC.SPSnalus) > 0 {
		d.sps = avcC.SPSnalus[0]
	}
	if len(avcC.PPSnalus) > 0 {
		d.pps = avcC.PPSnalus[0]
	}

	d.trackID = videoTrack.Tkhd.TrackID
	d.timeScale = videoTrack.Mdia.Mdhd.Timescale
	if initFile.Init.Moov.Mvex != nil {
		if trex, ok := initFile.Init.Moov.Mvex.GetTrex(d.trackID); ok {
			d.trex = trex
		}
	}
	d.baseTime = 0

	return nil
}

func (d *hdsMP4FFDemuxer) Demux(data []byte) error {
	if d.onFrame == nil {
		return nil
	}
	fragFile, err := mp4.DecodeFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	if len(fragFile.Segments) == 0 {
		return errors.New("mp4ff: no media segments in fragment")
	}

	for _, seg := range fragFile.Segments {
		for _, frag := range seg.Fragments {
			if frag.Moof == nil || frag.Mdat == nil {
				continue
			}
			for _, traf := range frag.Moof.Trafs {
				if traf.Tfhd == nil || traf.Tfhd.TrackID != d.trackID {
					continue
				}
				tfhd := traf.Tfhd
				baseOffset := uint64(0)
				if tfhd.HasBaseDataOffset() {
					baseOffset = tfhd.BaseDataOffset
				} else if tfhd.DefaultBaseIfMoof() {
					baseOffset = frag.Moof.StartPos
				}
				for _, trun := range traf.Truns {
					if trun == nil {
						continue
					}
					_ = trun.AddSampleDefaultValues(tfhd, d.trex)
					off := baseOffset
					if trun.HasDataOffset() {
						off = uint64(int64(off) + int64(trun.DataOffset))
					}
					offsetInMdat := uint32(0)
					if off >= frag.Mdat.PayloadAbsoluteOffset() {
						offsetInMdat = uint32(off - frag.Mdat.PayloadAbsoluteOffset())
					}

					samples := trun.GetFullSamples(offsetInMdat, traf.Tfdt.BaseMediaDecodeTime(), frag.Mdat)
					for _, sample := range samples {
						if len(sample.Data) == 0 {
							continue
						}
						decodeTime := sample.DecodeTime
						if d.baseTime == 0 {
							d.baseTime = decodeTime
						}
						if decodeTime >= d.baseTime {
							decodeTime -= d.baseTime
						}

						presTime := sample.PresentationTime()
						if presTime < 0 {
							presTime = int64(decodeTime)
						}
						if d.baseTime != 0 && presTime >= int64(d.baseTime) {
							presTime -= int64(d.baseTime)
						}

						pts := uint64(presTime)
						dts := decodeTime
						if d.timeScale != 0 && d.timeScale != 90000 {
							pts = pts * 90000 / uint64(d.timeScale)
							dts = dts * 90000 / uint64(d.timeScale)
						}

						nalus, err := splitAVCC(sample.Data)
						if err != nil {
							continue
						}
						key := sample.IsSync()
						d.onFrame(nalus, key, pts, dts)
					}
				}
			}
		}
	}
	return nil
}

func splitAVCC(sample []byte) ([][]byte, error) {
	var nalus [][]byte
	for pos := 0; pos+4 <= len(sample); {
		naluLen := int(binary.BigEndian.Uint32(sample[pos:]))
		pos += 4
		if naluLen <= 0 || pos+naluLen > len(sample) {
			return nil, fmt.Errorf("invalid AVCC length %d", naluLen)
		}
		nalu := sample[pos : pos+naluLen]
		pos += naluLen
		if len(nalu) > 0 {
			nalus = append(nalus, nalu)
		}
	}
	if len(nalus) == 0 {
		return nil, errors.New("no NALUs in sample")
	}
	return nalus, nil
}
