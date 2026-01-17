package homekit

import (
	"encoding/hex"

	"github.com/AlexxIT/go2rtc/pkg/aac"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/AlexxIT/go2rtc/pkg/h264"
	"github.com/AlexxIT/go2rtc/pkg/hap/camera"
)

var videoCodecs = [...]string{core.CodecH264}
var videoProfiles = [...]string{"4200", "4D00", "6400"}
var videoLevels = [...]string{"1F", "20", "28"}

func videoToMedia(codecs []camera.VideoCodecConfiguration) *core.Media {
	media := &core.Media{
		Kind: core.KindVideo, Direction: core.DirectionRecvonly,
	}

	for _, codec := range codecs {
		for _, param := range codec.CodecParams {
			// get best profile and level
			profileID := core.Max(param.ProfileID)
			level := core.Max(param.Level)
			profile := videoProfiles[profileID] + videoLevels[level]
			mediaCodec := &core.Codec{
				Name:      videoCodecs[codec.CodecType],
				ClockRate: 90000,
				FmtpLine:  "profile-level-id=" + profile,
			}
			media.Codecs = append(media.Codecs, mediaCodec)
		}
	}

	return media
}

var audioCodecs = [...]string{core.CodecPCMU, core.CodecPCMA, core.CodecELD, core.CodecOpus}
var audioSampleRates = [...]uint32{8000, 16000, 24000}

func audioToMedia(codecs []camera.AudioCodecConfiguration) *core.Media {
	media := &core.Media{
		Kind: core.KindAudio, Direction: core.DirectionRecvonly,
	}

	for _, codec := range codecs {
		for _, param := range codec.CodecParams {
			for _, sampleRate := range param.SampleRate {
				mediaCodec := &core.Codec{
					Name:      audioCodecs[codec.CodecType],
					ClockRate: audioSampleRates[sampleRate],
					Channels:  param.Channels,
				}

				if mediaCodec.Name == core.CodecELD {
					// only this version works with FFmpeg
					conf := aac.EncodeConfig(aac.TypeAACELD, 24000, 1, true)
					mediaCodec.FmtpLine = aac.FMTP + hex.EncodeToString(conf)
				}

				media.Codecs = append(media.Codecs, mediaCodec)
			}
		}
	}

	return media
}

func selectVideoConfig(track *core.Receiver, configs []camera.VideoCodecConfiguration, maxWidth, maxHeight int) *camera.VideoCodecConfiguration {
	if len(configs) == 0 {
		return nil
	}

	var (
		bestCfg    *camera.VideoCodecConfiguration
		bestAttrs  camera.VideoCodecAttributes
		bestArea   uint32
		bestPID    byte
		bestLevel  byte
		bestParams camera.VideoCodecParameters
		wantPID    *byte
		wantLevel  *byte
	)

	if track != nil {
		profile := h264.GetProfileLevelID(track.Codec.FmtpLine)
		if len(profile) >= 6 {
			for i, s := range videoProfiles {
				if s == profile[:4] {
					v := byte(i)
					wantPID = &v
					break
				}
			}
			for i, s := range videoLevels {
				if s == profile[4:] {
					v := byte(i)
					wantLevel = &v
					break
				}
			}
		}
	}

	for i := range configs {
		cfg := &configs[i]

		var attrs camera.VideoCodecAttributes
		for _, a := range cfg.VideoAttrs {
			if (maxWidth > 0 && int(a.Width) > maxWidth) || (maxHeight > 0 && int(a.Height) > maxHeight) {
				continue
			}
			if a.Width >= attrs.Width && a.Height >= attrs.Height {
				attrs = a
			}
		}

		if attrs.Width == 0 || attrs.Height == 0 {
			for _, a := range cfg.VideoAttrs {
				if a.Width >= attrs.Width && a.Height >= attrs.Height {
					attrs = a
				}
			}
		}

		var pid byte
		var level byte
		if len(cfg.CodecParams) > 0 {
			pid = core.Max(cfg.CodecParams[0].ProfileID)
			level = core.Max(cfg.CodecParams[0].Level)
			bestParams = cfg.CodecParams[0]
		}
		if wantPID != nil {
			for _, params := range cfg.CodecParams {
				for _, p := range params.ProfileID {
					if p == *wantPID {
						pid = p
					}
				}
			}
		}
		if wantLevel != nil {
			for _, params := range cfg.CodecParams {
				for _, l := range params.Level {
					if l == *wantLevel {
						level = l
					}
				}
			}
		}

		area := uint32(attrs.Width) * uint32(attrs.Height)
		if area > bestArea {
			bestArea = area
			bestCfg = cfg
			bestAttrs = attrs
			bestPID = pid
			bestLevel = level
			if len(cfg.CodecParams) > 0 {
				bestParams = cfg.CodecParams[0]
			}
		}
	}

	if bestCfg == nil {
		return &configs[0]
	}

	return &camera.VideoCodecConfiguration{
		CodecType: bestCfg.CodecType,
		CodecParams: []camera.VideoCodecParameters{
			{
				ProfileID: []byte{bestPID},
				Level:     []byte{bestLevel},
				PacketizationMode: bestParams.PacketizationMode,
				CVOEnabled:        bestParams.CVOEnabled,
				CVOID:             bestParams.CVOID,
			},
		},
		VideoAttrs: []camera.VideoCodecAttributes{bestAttrs},
	}
}

func trackToAudio(track *core.Receiver, audio0 *camera.AudioCodecConfiguration) *camera.AudioCodecConfiguration {
	codecType := audio0.CodecType
	channels := audio0.CodecParams[0].Channels
	sampleRate := audio0.CodecParams[0].SampleRate[0]

	if track != nil {
		channels = uint8(track.Codec.Channels)

		for i, s := range audioCodecs {
			if s == track.Codec.Name {
				codecType = byte(i)
				break
			}
		}

		for i, s := range audioSampleRates {
			if s == track.Codec.ClockRate {
				sampleRate = byte(i)
				break
			}
		}
	}

	return &camera.AudioCodecConfiguration{
		CodecType: codecType,
		CodecParams: []camera.AudioCodecParameters{
			{
				Channels:   channels,
				SampleRate: []byte{sampleRate},
				RTPTime:    []uint8{20},
			},
		},
	}
}
