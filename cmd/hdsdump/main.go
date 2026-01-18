package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/iso"
	"github.com/AlexxIT/go2rtc/pkg/mp4"
	"github.com/AlexxIT/go2rtc/pkg/mp4/fmp4"
	"github.com/pion/rtp"
)

func main() {
	initPath := flag.String("init", "", "path to init segment")
	windowPath := flag.String("window", "", "path to concatenated fragments")
	outPath := flag.String("out", "", "output mp4 file")
	flag.Parse()

	if *initPath == "" || *windowPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hdsdump -init <init.bin> -window <window.bin> -out <out.mp4>")
		os.Exit(2)
	}

	initData, err := os.ReadFile(*initPath)
	if err != nil {
		fail(err)
	}

	windowData, err := os.ReadFile(*windowPath)
	if err != nil {
		fail(err)
	}

	d := fmp4.NewDemuxer()
	if err := d.SetInit(initData); err != nil {
		fail(err)
	}
	if s := h264.DecodeSPS(d.SPS); s != nil {
		fmt.Printf("init SPS len=%d width=%d height=%d\n", len(d.SPS), s.Width(), s.Height())
	} else {
		fmt.Printf("init SPS len=%d decode=failed\n", len(d.SPS))
	}

	avcc := make([]byte, 0, len(d.SPS)+len(d.PPS)+8)
	avcc = append(avcc, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(d.SPS)))
	avcc = append(avcc, d.SPS...)
	avcc = append(avcc, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(avcc[len(avcc)-4:], uint32(len(d.PPS)))
	avcc = append(avcc, d.PPS...)
	codec := h264.AVCCToCodec(avcc)

	muxer := &mp4.Muxer{}
	muxer.AddTrack(codec)

	out, err := os.Create(*outPath)
	if err != nil {
		fail(err)
	}
	defer out.Close()

	init, err := muxer.GetInit()
	if err != nil {
		fail(err)
	}
	if _, err := out.Write(init); err != nil {
		fail(err)
	}

	frameCount := 0
	d.SetOnFrame(func(nalus [][]byte, keyframe bool, pts, dts uint64) {
		if frameCount < 5 {
			var types []byte
			for _, nalu := range nalus {
				if len(nalu) == 0 {
					continue
				}
				typ := nalu[0] & 0x1F
				types = append(types, typ)
				switch typ {
				case 7:
					if s := h264.DecodeSPS(nalu); s != nil {
						fmt.Printf("frame SPS len=%d width=%d height=%d\n", len(nalu), s.Width(), s.Height())
					} else {
						fmt.Printf("frame SPS len=%d decode=failed\n", len(nalu))
					}
				case 8:
					fmt.Printf("frame PPS len=%d\n", len(nalu))
				}
			}
			fmt.Printf("frame types=%v key=%v pts=%d\n", types, keyframe, pts)
			frameCount++
		}

		payload := make([]byte, 0, 64*1024)
		for _, nalu := range nalus {
			payload = append(payload, byte(len(nalu)>>24), byte(len(nalu)>>16), byte(len(nalu)>>8), byte(len(nalu)))
			payload = append(payload, nalu...)
		}

		var cts uint16
		if pts > dts && pts-dts <= 0xffff {
			cts = uint16(pts - dts)
		}

		pkt := &rtp.Packet{
			Header:  rtp.Header{Timestamp: uint32(pts), ExtensionProfile: cts},
			Payload: payload,
		}

		frag := muxer.GetPayload(0, pkt)
		if _, err := out.Write(frag); err != nil {
			fail(err)
		}
	})

	didDebug := false
	for _, frag := range splitFragments(windowData) {
		if !didDebug {
			if err := debugSamples(frag); err == nil {
				didDebug = true
			}
		}
		if err := d.Demux(frag); err != nil {
			fmt.Fprintf(os.Stderr, "demux error: %v\n", err)
		}
	}
}

func splitFragments(data []byte) [][]byte {
	var (
		frags      [][]byte
		start      = -1
		haveMoof   bool
		haveMdat   bool
		cursor     int
		dataLen    = len(data)
		currentEnd int
	)

	for cursor+8 <= dataLen {
		size := int(binary.BigEndian.Uint32(data[cursor:]))
		if size < 8 || cursor+size > dataLen {
			break
		}
		kind := string(data[cursor+4 : cursor+8])
		if kind == "moof" && !haveMoof {
			start = cursor
			haveMoof = true
		}
		if kind == "mdat" && haveMoof {
			haveMdat = true
			currentEnd = cursor + size
		}
		cursor += size
		if haveMoof && haveMdat && start >= 0 {
			frags = append(frags, data[start:currentEnd])
			haveMoof = false
			haveMdat = false
			start = -1
		}
	}

	return frags
}

func debugSamples(data []byte) error {
	atoms, err := iso.DecodeAtoms(data)
	if err != nil {
		return err
	}

	var truns []*iso.AtomTrun
	var mdat []byte
	var dataOffset uint32

	for _, atom := range atoms {
		switch a := atom.(type) {
		case *iso.AtomTrun:
			truns = append(truns, a)
		case *iso.AtomMdat:
			mdat = a.Data
		}
	}
	if len(truns) == 0 || mdat == nil {
		return fmt.Errorf("no trun/mdat")
	}

	var trun *iso.AtomTrun
	var best uint64
	for _, cand := range truns {
		var total uint64
		for _, size := range cand.SamplesSize {
			total += uint64(size)
		}
		if total > best {
			best = total
			trun = cand
			dataOffset = cand.DataOffset
		}
	}

	offset := resolveSampleOffset(dataOffset, data, mdat)
	limit := len(trun.SamplesSize)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		size := trun.SamplesSize[i]
		if int(offset)+int(size) > len(mdat) {
			break
		}
		sample := mdat[offset : offset+size]
		offset += size
		if len(sample) < 5 {
			fmt.Printf("sample%d size=%d (too short)\n", i, size)
			continue
		}
		nlen := binary.BigEndian.Uint32(sample[:4])
		typ := sample[4] & 0x1F
		prefix := sample
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		fmt.Printf("sample%d size=%d nlen=%d type=%d prefix=%x\n", i, size, nlen, typ, prefix)
	}
	return nil
}

func resolveSampleOffset(dataOffset uint32, fragment []byte, mdat []byte) uint32 {
	moofStart, mdatStart, okMoof, okMdat := findFragmentOffsets(fragment)
	if okMoof && okMdat {
		mdatDataStart := mdatStart + 8
		sampleStart := moofStart + dataOffset
		if sampleStart >= mdatDataStart {
			rel := sampleStart - mdatDataStart
			if rel < uint32(len(mdat)) {
				return rel
			}
		}
	}
	if dataOffset < uint32(len(mdat)) {
		return dataOffset
	}
	if dataOffset > 8 && dataOffset-8 < uint32(len(mdat)) {
		return dataOffset - 8
	}
	return 0
}

func findFragmentOffsets(data []byte) (moofStart uint32, mdatStart uint32, okMoof bool, okMdat bool) {
	var offset uint32
	for int(offset)+8 <= len(data) {
		size := binary.BigEndian.Uint32(data[offset:])
		if size < 8 || int(offset)+int(size) > len(data) {
			break
		}
		kind := string(data[offset+4 : offset+8])
		switch kind {
		case "moof":
			moofStart = offset
			okMoof = true
		case "mdat":
			mdatStart = offset
			okMdat = true
		}
		if okMoof && okMdat {
			break
		}
		offset += size
	}
	return moofStart, mdatStart, okMoof, okMdat
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
