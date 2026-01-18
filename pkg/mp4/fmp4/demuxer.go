// Package fmp4 provides fragmented MP4 (fMP4) demuxing support for HDS video streaming
package fmp4

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"

	"github.com/AlexxIT/go2rtc/pkg/h264/annexb"
	"github.com/AlexxIT/go2rtc/pkg/iso"
)

// Demuxer demuxes fragmented MP4 streams into NAL units
type Demuxer struct {
	// Codec configuration from moov box
	VideoCodec string // "h264" or "h265"
	SPS        []byte
	PPS        []byte
	VPS        []byte // H.265 only

	// Track information
	trackID      uint32
	timeScale    uint32
	timeScales   map[uint32]uint32
	spsByTrack   map[uint32][]byte
	ppsByTrack   map[uint32][]byte
	vpsByTrack   map[uint32][]byte
	codecByTrack map[uint32]string
	naluLen      int
	loggedOffset bool
	loggedSample bool
	loggedTrack  bool
	loggedTrun   bool

	// Callback for decoded frames
	onFrame func(nalus [][]byte, keyframe bool, pts, dts uint64)
}

// NewDemuxer creates a new fMP4 demuxer
func NewDemuxer() *Demuxer {
	return &Demuxer{}
}

// SetOnFrame sets the callback for decoded frames
func (d *Demuxer) SetOnFrame(fn func(nalus [][]byte, keyframe bool, pts, dts uint64)) {
	d.onFrame = fn
}

// SetInit processes the initialization segment (ftyp + moov)
// This extracts codec configuration (SPS/PPS/VPS) from the moov box
func (d *Demuxer) SetInit(data []byte) error {
	atoms, err := iso.DecodeAtoms(data)
	if err != nil {
		return fmt.Errorf("failed to decode init atoms: %w", err)
	}

	if d.timeScales == nil {
		d.timeScales = make(map[uint32]uint32)
	}
	if d.spsByTrack == nil {
		d.spsByTrack = make(map[uint32][]byte)
		d.ppsByTrack = make(map[uint32][]byte)
		d.vpsByTrack = make(map[uint32][]byte)
		d.codecByTrack = make(map[uint32]string)
	}

	var currentTrackID uint32
	for _, atom := range atoms {
		switch a := atom.(type) {
		case *iso.AtomTkhd:
			currentTrackID = a.TrackID
		case *iso.AtomMdhd:
			if currentTrackID != 0 {
				d.timeScales[currentTrackID] = a.TimeScale
			}
		case *iso.AtomVideo:
			switch a.Name {
			case "avc1":
				d.VideoCodec = "h264"
				if currentTrackID != 0 {
					d.trackID = currentTrackID
					if ts := d.timeScales[currentTrackID]; ts != 0 {
						d.timeScale = ts
					}
				}
				// Parse avcC box to extract SPS/PPS
				if err := d.parseAVCC(a.Config); err != nil {
					return fmt.Errorf("failed to parse avcC: %w", err)
				}
				if currentTrackID != 0 {
					d.spsByTrack[currentTrackID] = append([]byte(nil), d.SPS...)
					d.ppsByTrack[currentTrackID] = append([]byte(nil), d.PPS...)
					d.codecByTrack[currentTrackID] = d.VideoCodec
				}
			case "hev1", "hvc1":
				d.VideoCodec = "h265"
				if currentTrackID != 0 {
					d.trackID = currentTrackID
					if ts := d.timeScales[currentTrackID]; ts != 0 {
						d.timeScale = ts
					}
				}
				// Parse hvcC box to extract VPS/SPS/PPS
				if err := d.parseHVCC(a.Config); err != nil {
					return fmt.Errorf("failed to parse hvcC: %w", err)
				}
				if currentTrackID != 0 {
					d.spsByTrack[currentTrackID] = append([]byte(nil), d.SPS...)
					d.ppsByTrack[currentTrackID] = append([]byte(nil), d.PPS...)
					d.vpsByTrack[currentTrackID] = append([]byte(nil), d.VPS...)
					d.codecByTrack[currentTrackID] = d.VideoCodec
				}
			default:
				return fmt.Errorf("unsupported video codec: %s", a.Name)
			}
		}
	}

	if d.VideoCodec == "" {
		return fmt.Errorf("no video codec found in init segment")
	}

	return nil
}

// parseAVCC extracts SPS/PPS from H.264 avcC box
func (d *Demuxer) parseAVCC(config []byte) error {
	if len(config) < 7 {
		return fmt.Errorf("avcC box too short")
	}

	// avcC format:
	// configurationVersion (1)
	// AVCProfileIndication (1)
	// profile_compatibility (1)
	// AVCLevelIndication (1)
	// lengthSizeMinusOne (1)
	// numOfSequenceParameterSets (1)
	// ... SPS entries ...
	// numOfPictureParameterSets (1)
	// ... PPS entries ...

	// lengthSizeMinusOne stored in low 2 bits of byte 4
	d.naluLen = int(config[4]&0x03) + 1
	pos := 5 // skip first 5 bytes

	// Read SPS count
	numSPS := config[pos] & 0x1F
	pos++

	// Read SPS
	for i := 0; i < int(numSPS); i++ {
		if pos+2 > len(config) {
			return fmt.Errorf("invalid avcC: truncated SPS")
		}
		spsLen := int(config[pos])<<8 | int(config[pos+1])
		pos += 2

		if pos+spsLen > len(config) {
			return fmt.Errorf("invalid avcC: truncated SPS data")
		}
		d.SPS = config[pos : pos+spsLen]
		pos += spsLen
		break // only use first SPS
	}

	// Read PPS count
	if pos >= len(config) {
		return fmt.Errorf("invalid avcC: missing PPS count")
	}
	numPPS := config[pos]
	pos++

	// Read PPS
	for i := 0; i < int(numPPS); i++ {
		if pos+2 > len(config) {
			return fmt.Errorf("invalid avcC: truncated PPS")
		}
		ppsLen := int(config[pos])<<8 | int(config[pos+1])
		pos += 2

		if pos+ppsLen > len(config) {
			return fmt.Errorf("invalid avcC: truncated PPS data")
		}
		d.PPS = config[pos : pos+ppsLen]
		pos += ppsLen
		break // only use first PPS
	}

	return nil
}

// parseHVCC extracts VPS/SPS/PPS from H.265 hvcC box
func (d *Demuxer) parseHVCC(config []byte) error {
	if len(config) < 23 {
		return fmt.Errorf("hvcC box too short")
	}

	// hvcC format is complex, simplified parsing:
	// lengthSizeMinusOne stored in low 2 bits of byte 21
	d.naluLen = int(config[21]&0x03) + 1
	// Skip to numOfArrays at byte 22
	pos := 22
	numArrays := int(config[pos])
	pos++

	for i := 0; i < numArrays; i++ {
		if pos+3 > len(config) {
			return fmt.Errorf("invalid hvcC: truncated array header")
		}

		nalType := config[pos] & 0x3F
		pos++

		numNalus := int(config[pos])<<8 | int(config[pos+1])
		pos += 2

		for j := 0; j < numNalus; j++ {
			if pos+2 > len(config) {
				return fmt.Errorf("invalid hvcC: truncated NAL length")
			}
			naluLen := int(config[pos])<<8 | int(config[pos+1])
			pos += 2

			if pos+naluLen > len(config) {
				return fmt.Errorf("invalid hvcC: truncated NAL data")
			}
			nalu := config[pos : pos+naluLen]
			pos += naluLen

			// VPS = 32, SPS = 33, PPS = 34
			switch nalType {
			case 32: // VPS
				d.VPS = nalu
			case 33: // SPS
				d.SPS = nalu
			case 34: // PPS
				d.PPS = nalu
			}
		}
	}

	return nil
}

// Demux processes a media fragment (moof + mdat)
// Extracts NAL units and calls onFrame callback
func (d *Demuxer) Demux(data []byte) error {
	if d.onFrame == nil {
		return nil // no callback set, nothing to do
	}

	atoms, err := iso.DecodeAtoms(data)
	if err != nil {
		return fmt.Errorf("failed to decode fragment atoms: %w", err)
	}

	var decodeTime uint64
	type trunCandidate struct {
		trun              *iso.AtomTrun
		trackID           uint32
		defaultSampleSize uint32
		decodeTime        uint64
	}

	var trun *iso.AtomTrun
	var mdatData []byte
	var currentTrackID uint32
	var currentDefaultSampleSize uint32
	var currentDecodeTime uint64
	var videoDefaultSampleSize uint32
	var candidates []trunCandidate

	for _, atom := range atoms {
		switch a := atom.(type) {
		case *iso.AtomTfhd:
			currentTrackID = a.TrackID
			currentDefaultSampleSize = a.SampleSize
			currentDecodeTime = 0
			// Track ID from init may not align with fragment track IDs for some cameras.
			// If it differs, prefer the init value unless we don't have one.
			if d.trackID == 0 {
				d.trackID = a.TrackID
			}
			if a.TrackID == d.trackID && a.SampleSize != 0 {
				videoDefaultSampleSize = a.SampleSize
			}
		case *iso.AtomTfdt:
			currentDecodeTime = a.DecodeTime
		case *iso.AtomTrun:
			candidates = append(candidates, trunCandidate{
				trun:              a,
				trackID:           currentTrackID,
				defaultSampleSize: currentDefaultSampleSize,
				decodeTime:        currentDecodeTime,
			})
		case *iso.AtomMdat:
			mdatData = a.Data
		}
	}

	if len(candidates) == 0 || mdatData == nil {
		return fmt.Errorf("missing trun or mdat in fragment")
	}

	best := trunCandidate{}
	bestSize := uint64(0)
	for _, cand := range candidates {
		size := estimateTrunBytes(cand.trun, cand.defaultSampleSize)
		if size > bestSize {
			best = cand
			bestSize = size
		}
	}

	if d.trackID != 0 {
		var match trunCandidate
		var matchSize uint64
		for _, cand := range candidates {
			if cand.trackID != d.trackID {
				continue
			}
			size := estimateTrunBytes(cand.trun, cand.defaultSampleSize)
			if size > matchSize {
				match = cand
				matchSize = size
			}
		}

		// If the matching track looks wrong, fall back to the largest sample set.
		if match.trun != nil && bestSize <= matchSize*2 {
			trun = match.trun
			videoDefaultSampleSize = match.defaultSampleSize
			decodeTime = match.decodeTime
		} else {
			if match.trun != nil && !d.loggedTrack {
				log.Printf("[fmp4] track id mismatch init=%d fragment=%d", d.trackID, best.trackID)
				d.loggedTrack = true
			}
			trun = best.trun
			videoDefaultSampleSize = best.defaultSampleSize
			decodeTime = best.decodeTime
			if best.trackID != 0 && best.trackID != d.trackID {
				d.trackID = best.trackID
			}
		}
	} else {
		trun = best.trun
		videoDefaultSampleSize = best.defaultSampleSize
		decodeTime = best.decodeTime
		if best.trackID != 0 {
			d.trackID = best.trackID
		}
	}

	if d.trackID != 0 {
		if ts := d.timeScales[d.trackID]; ts != 0 {
			d.timeScale = ts
		}
		if codec := d.codecByTrack[d.trackID]; codec != "" {
			d.VideoCodec = codec
		}
		if sps := d.spsByTrack[d.trackID]; len(sps) > 0 {
			d.SPS = sps
		}
		if pps := d.ppsByTrack[d.trackID]; len(pps) > 0 {
			d.PPS = pps
		}
		if vps := d.vpsByTrack[d.trackID]; len(vps) > 0 {
			d.VPS = vps
		}
	}

	if !d.loggedTrun {
		var total uint64
		var max uint32
		limit := len(trun.SamplesSize)
		if limit > 8 {
			limit = 8
		}
		for _, size := range trun.SamplesSize {
			total += uint64(size)
			if size > max {
				max = size
			}
		}
		log.Printf("[fmp4] trun samples=%d total=%d max=%d mdat=%d", len(trun.SamplesSize), total, max, len(mdatData))
		if limit > 0 {
			log.Printf("[fmp4] trun sizes head=%v", trun.SamplesSize[:limit])
		}
		d.loggedTrun = true
	}

	if trun != nil && len(trun.SamplesSize) == 0 && trun.SamplesCount > 0 && videoDefaultSampleSize != 0 {
		trun.SamplesSize = make([]uint32, trun.SamplesCount)
		for i := range trun.SamplesSize {
			trun.SamplesSize[i] = videoDefaultSampleSize
		}
	}

	sampleOffset := uint32(0)
	if trun.DataOffset > 0 {
		sampleOffset = d.resolveSampleOffset(trun.DataOffset, data, mdatData)
		if !d.loggedOffset {
			log.Printf("[fmp4] trun data offset=%d resolved=%d mdat=%d defaultSample=%d", trun.DataOffset, sampleOffset, len(mdatData), videoDefaultSampleSize)
			d.loggedOffset = true
		}
	}

	// Process each sample in the trun
	return d.processSamples(trun, mdatData, decodeTime, sampleOffset)
}

// processSamples extracts NAL units from mdat based on trun sample info
func (d *Demuxer) processSamples(trun *iso.AtomTrun, mdat []byte, baseTime uint64, offset uint32) error {
	if len(trun.SamplesSize) == 0 {
		return fmt.Errorf("no sample sizes in trun")
	}

	numSamples := len(trun.SamplesSize)
	currentTime := baseTime

	for i := 0; i < numSamples; i++ {
		sampleSize := trun.SamplesSize[i]
		if offset+sampleSize > uint32(len(mdat)) {
			return fmt.Errorf("sample %d exceeds mdat bounds", i)
		}

		sampleData := mdat[offset : offset+sampleSize]
		offset += sampleSize

		// Determine if this is a keyframe
		var sampleFlags uint32
		if i == 0 && trun.FirstSampleFlags != 0 {
			sampleFlags = trun.FirstSampleFlags
		} else if len(trun.SamplesFlags) > i {
			sampleFlags = trun.SamplesFlags[i]
		}

		keyframe := (sampleFlags & iso.SampleVideoNonIFrame) == 0

		if !d.loggedSample {
			prefix := sampleData
			if len(prefix) > 16 {
				prefix = prefix[:16]
			}
			log.Printf("[fmp4] sample0 size=%d offset=%d prefix=%s", sampleSize, offset-uint32(sampleSize), hex.EncodeToString(prefix))
			d.loggedSample = true
		}

		// Extract NAL units from sample
		nalus, err := d.extractNALUs(sampleData, keyframe)
		if err != nil {
			return fmt.Errorf("failed to extract NALUs from sample %d: %w", i, err)
		}

		// Calculate PTS/DTS
		var duration uint32
		if len(trun.SamplesDuration) > i {
			duration = trun.SamplesDuration[i]
		}

		var cts uint32
		if len(trun.SamplesCTS) > i {
			cts = trun.SamplesCTS[i]
		}

		dts := currentTime
		pts := currentTime + uint64(cts)

		if d.timeScale != 0 && d.timeScale != 90000 {
			dts = dts * 90000 / uint64(d.timeScale)
			pts = pts * 90000 / uint64(d.timeScale)
		}

		// Call frame callback
		d.onFrame(nalus, keyframe, pts, dts)

		currentTime += uint64(duration)
	}

	return nil
}

func estimateTrunBytes(trun *iso.AtomTrun, defaultSampleSize uint32) uint64 {
	if trun == nil {
		return 0
	}
	if len(trun.SamplesSize) > 0 {
		var total uint64
		for _, size := range trun.SamplesSize {
			total += uint64(size)
		}
		return total
	}
	if trun.SamplesCount > 0 && defaultSampleSize > 0 {
		return uint64(trun.SamplesCount) * uint64(defaultSampleSize)
	}
	return 0
}

func (d *Demuxer) resolveSampleOffset(dataOffset uint32, fragment []byte, mdat []byte) uint32 {
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

// extractNALUs extracts NAL units from a sample
// Samples in fMP4 use length-prefixed format (not Annex B)
func (d *Demuxer) extractNALUs(sample []byte, keyframe bool) ([][]byte, error) {
	var nalus [][]byte

	// Annex B samples include start codes.
	if containsStartCode(sample) {
		for _, nalu := range splitAnnexB(sample) {
			nalus = append(nalus, nalu)
		}
		return nalus, nil
	}

	// Extract NAL units from length-prefixed format
	// Format: [len][NALU]... with len size from avcC/hvcC
	nlen := d.naluLen
	if nlen < 1 || nlen > 4 {
		nlen = 4
	}
	nalus, err := parseAVCCWithLen(sample, nlen)
	if err == nil {
		return nalus, nil
	}

	// If parsing failed, try other length sizes as fallback.
	for _, alt := range []int{1, 2, 3, 4} {
		if alt == nlen {
			continue
		}
		if alt == 0 || alt > 4 {
			continue
		}
		if alts, altErr := parseAVCCWithLen(sample, alt); altErr == nil {
			return alts, nil
		}
	}

	// If the payload contains start codes, fall back to AnnexB splitting.
	if containsStartCode(sample) {
		for _, nalu := range splitAnnexB(sample) {
			nalus = append(nalus, nalu)
		}
		if len(nalus) > 0 {
			return nalus, nil
		}
	}

	if !d.loggedSample {
		d.loggedSample = true
		prefix := sample
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		log.Printf("[fmp4] nalu parse failed len=%d nlen=%d prefix=%s err=%v",
			len(sample), nlen, hex.EncodeToString(prefix), err,
		)
	}

	return nil, err
}

func isAnnexB(b []byte) bool {
	if len(b) < 3 {
		return false
	}
	if b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return true
	}
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return true
	}
	return false
}

func containsStartCode(b []byte) bool {
	for i := 0; i+3 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			return true
		}
		if i+4 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			return true
		}
	}
	return false
}

func parseAVCCWithLen(sample []byte, nlen int) ([][]byte, error) {
	var nalus [][]byte
	pos := 0
	for pos+nlen <= len(sample) {
		naluLen := 0
		for i := 0; i < nlen; i++ {
			naluLen = (naluLen << 8) | int(sample[pos+i])
		}
		pos += nlen

		if pos+naluLen > len(sample) {
			return nil, fmt.Errorf("NALU length %d exceeds sample bounds", naluLen)
		}

		nalu := sample[pos : pos+naluLen]
		pos += naluLen

		nalus = append(nalus, nalu)
	}

	return nalus, nil
}

func splitAnnexB(b []byte) [][]byte {
	var nalus [][]byte
	start := -1
	i := 0
	for i < len(b) {
		var sc int
		if i+3 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			sc = 3
		} else if i+4 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 0 && b[i+3] == 1 {
			sc = 4
		}
		if sc != 0 {
			if start != -1 && i > start {
				nalus = append(nalus, b[start:i])
			}
			i += sc
			start = i
			continue
		}
		i++
	}
	if start != -1 && start < len(b) {
		nalus = append(nalus, b[start:])
	}
	return nalus
}

// GetSPS returns the SPS (Sequence Parameter Set) for H.264/H.265
func (d *Demuxer) GetSPS() []byte {
	return d.SPS
}

// GetPPS returns the PPS (Picture Parameter Set) for H.264/H.265
func (d *Demuxer) GetPPS() []byte {
	return d.PPS
}

// GetVPS returns the VPS (Video Parameter Set) for H.265 only
func (d *Demuxer) GetVPS() []byte {
	return d.VPS
}

// GetAnnexB converts the stored parameter sets to Annex B format
// This is useful for formats that require Annex B (like RTP)
func (d *Demuxer) GetAnnexB() []byte {
	var result []byte

	switch d.VideoCodec {
	case "h264":
		if d.SPS != nil {
			result = append(result, []byte(annexb.StartCode)...)
			result = append(result, d.SPS...)
		}
		if d.PPS != nil {
			result = append(result, []byte(annexb.StartCode)...)
			result = append(result, d.PPS...)
		}
	case "h265":
		if d.VPS != nil {
			result = append(result, []byte(annexb.StartCode)...)
			result = append(result, d.VPS...)
		}
		if d.SPS != nil {
			result = append(result, []byte(annexb.StartCode)...)
			result = append(result, d.SPS...)
		}
		if d.PPS != nil {
			result = append(result, []byte(annexb.StartCode)...)
			result = append(result, d.PPS...)
		}
	}

	return result
}

// NALUsToAnnexB converts NAL units to Annex B format
func NALUsToAnnexB(nalus [][]byte) []byte {
	var result []byte
	for _, nalu := range nalus {
		result = append(result, []byte(annexb.StartCode)...)
		result = append(result, nalu...)
	}
	return result
}
