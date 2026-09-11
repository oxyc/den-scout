package scout

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Release names as indexers actually spell them, through the whole title path: codec, audio family, bit
// depth and the web hint they add up to. The container comes from the name's extension, as it does for
// an unprobed release.
func TestStreamAttributes_webFromRealisticTitles(t *testing.T) {
	for _, c := range []struct {
		title        string
		codec, audio string
		depth        int
		direct       []string
		remux        string
	}{
		{"The.Movie.2023.1080p.WEB-DL.DDP5.1.H.264-FLUX", "h264", "eac3", 0, []string{}, "audio"},
		{"Movie.2019.1080p.BluRay.DTS-HD.MA.5.1.x264-GRP.mkv", "h264", "dtshdma", 0, []string{}, "audio"},
		{"Movie.2021.2160p.UHD.BluRay.REMUX.HEVC.DV.HDR.TrueHD.Atmos.7.1-GRP.mkv", "hevc", "truehd", 10, []string{}, "audio"},
		{"Movie.2020.1080p.WEBRip.x264.AAC5.1-[YTS.MX].mp4", "h264", "aac", 0, []string{"safari", "chrome"}, "copy"},
		{"Show.S01E01.1080p.WEB-DL.x265.10bit.AAC5.1-GRP.mkv", "hevc", "aac", 10, []string{"chrome"}, "copy"},
		{"Movie.2022.1080p.WEBRip.x265.10bit.Opus.5.1-GRP.mkv", "hevc", "opus", 10, []string{"chrome"}, "audio"},
		// Hi10P names no codec but is H.264 High 10 by definition, which no Safari decodes.
		{"[Group] Anime - 01 [BD 1080p Hi10P FLAC].mkv", "h264", "flac", 10, []string{"chrome"}, "video"},
		// DV with no HDR10 named is read as profile 5: no base layer Chrome can show.
		{"Movie.2023.2160p.WEB-DL.DV.HEVC.AAC.mp4", "hevc", "aac", 10, []string{"safari"}, "copy"},
		{"Movie.2023.2160p.WEB-DL.DV.HDR10.HEVC.AAC.mp4", "hevc", "aac", 10, []string{"safari", "chrome"}, "copy"},
		{"Movie.2024.1080p.WEB-DL.AV1.Opus.webm", "av1", "opus", 0, []string{"chrome"}, "video"},
		// The title path doesn't name XviD, so there is no codec to answer from.
		{"Movie.2005.DVDRip.XviD-MP3.avi", "", "mp3", 0, []string{}, "none"},
	} {
		t.Run(c.title, func(t *testing.T) {
			a := streamAttributes(RawStream{Title: c.title})
			if deref(a.Codec) != c.codec || deref(a.AudioCodec) != c.audio || a.BitDepth != c.depth {
				t.Errorf("codec/audio/depth = %q/%q/%d, want %q/%q/%d",
					deref(a.Codec), deref(a.AudioCodec), a.BitDepth, c.codec, c.audio, c.depth)
			}
			if !reflect.DeepEqual(a.Web.Direct, c.direct) || a.Web.Remux != c.remux {
				t.Errorf("web = %+v, want direct=%v remux=%q", a.Web, c.direct, c.remux)
			}
		})
	}
}

// The new audio branches, and the names that must NOT trip them.
func TestDetectAudioCodec_browserCodecs(t *testing.T) {
	for title, want := range map[string]string{
		"movie 2020 1080p web-dl aac2.0 h264":         "aac",
		"movie 2020 1080p web-dl he-aac x264":         "aac",
		"movie 2020 1080p bluray x264 mp3":            "mp3",
		"show s01e01 1080p hevc opus":                 "opus",
		"movie 2020 1080p web-dl ddp5.1 aac2.0 h.264": "eac3", // DD+ is the main track; the AAC is the companion
		"movie 2020 1080p bluray ac3 aac x264":        "ac3",
		"mr.hollands.opus.1995.1080p.bluray.x264-grp": "",
		"opus.2025.2160p.web-dl.x265":                 "",
		"opus.2025.1080p.web-dl.aac.x264":             "aac",
		"isaac.2020.1080p.web-dl.x264":                "",
		"aachen.story.2019.1080p.x264":                "",
	} {
		if got := detectAudioCodec(title); got != want {
			t.Errorf("detectAudioCodec(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestDetectBitDepth(t *testing.T) {
	for title, want := range map[string]int{
		"movie 1080p x265 10bit":            10,
		"movie 1080p x265 10-bit":           10,
		"movie 1080p hevc 10 bit":           10,
		"movie 1080p x265 10bits":           10,
		"movie 1080p hi10":                  10,
		"movie 1080p main10 hevc":           10,
		"movie 1080p bluray x264":           0,
		"show s01e10.bitter.end.1080p":      0, // "10.bit" inside an episode number and a word
		"movie 2010 bit part 1080p x264":    0,
		"movie 2160p web-dl hdr10 hevc":     10, // HDR is 10-bit by definition
		"movie 2160p web-dl dv hevc ddp5.1": 10,
	} {
		a := streamAttributes(RawStream{Title: title})
		if a.BitDepth != want {
			t.Errorf("bitDepth(%q) = %d, want %d", title, a.BitDepth, want)
		}
	}
}

// What the file says replaces what the title guessed, in both directions, and the web hint follows it.
func TestWebHint_followsTheProbe(t *testing.T) {
	for _, c := range []struct {
		name   string
		title  string
		probe  Probe
		direct []string
		remux  string
	}{
		{"High 10 in MP4, named plain x264", "Movie 1080p x264 AAC", Probe{Container: "mp4", VideoCodec: "h264", BitDepth: 10},
			[]string{"chrome"}, "video"},
		{"8-bit probe beats an HDR title's 10", "Movie 1080p HDR x264 AAC", Probe{Container: "mp4", VideoCodec: "h264", BitDepth: 8},
			[]string{"safari", "chrome"}, "copy"},
		{"DV profile 5 in Matroska", "Movie 2160p AAC", Probe{Container: "matroska", VideoCodec: "hevc", DolbyVision: true, DVProfile: 5},
			[]string{}, "copy"},
		{"DV profile 5 in MP4", "Movie 2160p AAC", Probe{Container: "mp4", VideoCodec: "hevc", DolbyVision: true, DVProfile: 5},
			[]string{"safari"}, "copy"},
		// A profile read from the file is trusted over the "no HDR10 named" guess.
		{"DV profile 8, title says only DV", "Movie 2160p DV AAC", Probe{Container: "mp4", VideoCodec: "hevc", DolbyVision: true, DVProfile: 8},
			[]string{"safari", "chrome"}, "copy"},
		{"DV profile 7 in MP4", "Movie 2160p AAC", Probe{Container: "mp4", VideoCodec: "hevc", DolbyVision: true, DVProfile: 7},
			[]string{"chrome"}, "copy"},
		{"XviD in AVI", "Movie DVDRip MP3", Probe{Container: "avi", VideoCodec: "mpeg4"}, []string{}, "video"},
		{"VP9 in Matroska", "Movie 1080p WEB-DL Opus", Probe{Container: "matroska", VideoCodec: "vp9"}, []string{"chrome"}, "video"},
		{"MP4 named as MKV", "Movie.1080p.x264.AAC.mkv", Probe{Container: "mp4"}, []string{"safari", "chrome"}, "copy"},
		{"audio the title doesn't name", "Movie 1080p x264", Probe{Container: "mp4"}, []string{}, "audio"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := c.probe
			a := streamAttributes(RawStream{Title: c.title, Probe: &p})
			if !reflect.DeepEqual(a.Web.Direct, c.direct) || a.Web.Remux != c.remux {
				t.Errorf("web = %+v, want direct=%v remux=%q", a.Web, c.direct, c.remux)
			}
			if p.BitDepth != 0 && a.BitDepth != p.BitDepth {
				t.Errorf("bitDepth = %d, the probe said %d", a.BitDepth, p.BitDepth)
			}
			if p.DVProfile != 0 && a.DVProfile != p.DVProfile {
				t.Errorf("dvProfile = %d, the probe said %d", a.DVProfile, p.DVProfile)
			}
		})
	}
}

// The new fields only ADD to the wire: unknown depth and profile are omitted, and `direct` is an empty
// list rather than null, so a client can test membership without a nil check.
func TestStreamAttributes_webWireShape(t *testing.T) {
	body, err := json.Marshal(streamAttributes(RawStream{Title: "Movie 1080p"}))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["bitDepth"]; ok {
		t.Error("an unknown bit depth must be omitted, not sent as 0")
	}
	if _, ok := got["dvProfile"]; ok {
		t.Error("an unknown DV profile must be omitted, not sent as 0")
	}
	web, _ := got["web"].(map[string]any)
	if d, ok := web["direct"].([]any); !ok || len(d) != 0 || web["remux"] != "none" {
		t.Errorf("web = %v, want {direct: [], remux: none}", got["web"])
	}
}

func TestContainerOf(t *testing.T) {
	for title, want := range map[string]string{
		"Movie.mp4": "mp4", "Movie.M4V": "mp4", "Movie.mkv": "matroska", "Movie.webm": "webm",
		"Movie.avi": "avi", "Movie 1080p": "",
	} {
		if got := containerOf(RawStream{Title: title}); got != want {
			t.Errorf("containerOf(%q) = %q, want %q", title, got, want)
		}
	}
}

// Real files from ffmpeg (testdata/README.md): the bit depth comes from the configuration record the
// muxer wrote, in MP4's hvcC and in Matroska's CodecPrivate, not from a name.
func TestParseHead_bitDepthFromRealFiles(t *testing.T) {
	for _, c := range []struct {
		file, container, codec string
		depth                  int
	}{
		{"hevc10.mp4", "mp4", "hevc", 10},     // hvcC bitDepthLumaMinus8 = 2
		{"hi10p.mkv", "matroska", "h264", 10}, // CodecPrivate avcC, profile_idc 110
		{"sample.mp4", "mp4", "h264", 8},      // Constrained Baseline
		// High 4:4:4 Predictive allows 8 or 10 bits and only the SPS says which: unknown, not a guess.
		{"multi-audio.mp4", "mp4", "h264", 0},
	} {
		t.Run(c.file, func(t *testing.T) {
			p, err := ParseHead(readFixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			if p.Container != c.container || p.VideoCodec != c.codec || p.BitDepth != c.depth {
				t.Errorf("got %s/%s/%d, want %s/%s/%d", p.Container, p.VideoCodec, p.BitDepth, c.container, c.codec, c.depth)
			}
			if p.DolbyVision || p.DVProfile != 0 {
				t.Errorf("no Dolby Vision in this file: dv=%v profile=%d", p.DolbyVision, p.DVProfile)
			}
		})
	}
}

// DOVIDecoderConfigurationRecord bytes, laid out as doviProfile documents. byte 3 = level 6 in its high
// 5 bits plus the rpu/el/bl present flags; byte 4's high nibble is the base-layer compatibility id.
var (
	doviP5  = []byte{0x01, 0x00, 0x0A, 0x35, 0x00} // profile 5, level 6, rpu+bl, compat 0 (no fallback)
	doviP7  = []byte{0x01, 0x00, 0x0E, 0x37, 0x60} // profile 7, level 6, rpu+el+bl, compat 6 (HDR10 base)
	doviP81 = []byte{0x01, 0x00, 0x10, 0x35, 0x10} // profile 8, level 6, rpu+bl, compat 1 (HDR10 base)
)

// Dolby Vision can't be minted from a test source (ffmpeg needs an RPU to write one), so the record
// parser is pinned byte by byte instead.
func TestDoviProfile(t *testing.T) {
	for name, c := range map[string]struct {
		record []byte
		want   int
	}{
		"profile 5":   {doviP5, 5},
		"profile 7":   {doviP7, 7},
		"profile 8.1": {doviP81, 8},
		// dv_level's top bit shares byte 2 with the profile and must not leak into it.
		"level bit set":  {[]byte{0x01, 0x00, 0x11, 0x05}, 8},
		"too short":      {[]byte{0x01, 0x00, 0x10}, 0},
		"missing record": {nil, 0},
	} {
		if got := doviProfile(c.record); got != c.want {
			t.Errorf("%s: doviProfile = %d, want %d", name, got, c.want)
		}
	}
}

// An hvcC record whose bitDepthLumaMinus8 byte carries the reserved high bits a muxer sets (0b11111).
func hvcC(depth int) []byte {
	rec := make([]byte, 23)
	rec[0], rec[1] = 1, 2 // version 1, Main 10
	rec[17] = 0xF8 | byte(depth-8)
	return rec
}

func avcC(profile byte) []byte { return []byte{1, profile, 0, 40, 0xFF, 0xE0} }

func TestParseMP4_videoConfigBoxes(t *testing.T) {
	t.Run("DV profile 5 sample entry with hvcC and dvcC", func(t *testing.T) {
		p, ok := parseMP4(mp4File(trak("vide", "und", 0,
			videoEntry("dvh1", mp4box("hvcC", hvcC(10)), mp4box("dvcC", doviP5)))))
		if !ok || p.VideoCodec != "hevc" || p.BitDepth != 10 || !p.DolbyVision || p.DVProfile != 5 {
			t.Errorf("got %+v (ok=%v), want hevc, 10-bit, DV profile 5", p, ok)
		}
	})
	t.Run("High 10 avcC", func(t *testing.T) {
		p, _ := parseMP4(mp4File(trak("vide", "und", 0, videoEntry("avc1", mp4box("avcC", avcC(110))))))
		if p.BitDepth != 10 {
			t.Errorf("bitDepth = %d, want 10 from profile_idc 110", p.BitDepth)
		}
	})
	t.Run("dvwC", func(t *testing.T) {
		p, _ := parseMP4(mp4File(trak("vide", "und", 0, videoEntry("hvc1", mp4box("dvwC", []byte{1, 0, 20 << 1, 0}))))) // profile 20
		if !p.DolbyVision || p.DVProfile != 20 {
			t.Errorf("dv=%v profile=%d, want the dvwC record read", p.DolbyVision, p.DVProfile)
		}
	})
}

func TestAVCAndHEVCBitDepth(t *testing.T) {
	for profile, want := range map[byte]int{66: 8, 77: 8, 88: 8, 100: 8, 110: 10, 122: 0, 244: 0} {
		if got := avcBitDepth(avcC(profile)); got != want {
			t.Errorf("avcBitDepth(profile %d) = %d, want %d", profile, got, want)
		}
	}
	if avcBitDepth([]byte{1}) != 0 || hevcBitDepth(make([]byte, 17)) != 0 {
		t.Error("a truncated record must read as unknown")
	}
	if hevcBitDepth(hvcC(8)) != 8 || hevcBitDepth(hvcC(10)) != 10 {
		t.Error("hvcC bit depth mis-read")
	}
	if bitDepthFromConfig("av1", hvcC(10)) != 0 {
		t.Error("a codec with no parser here must read as unknown")
	}
}

// ebml assembles one element: its id's own bytes, an 8-byte size, then the body.
func ebml(id uint32, body ...[]byte) []byte {
	var out []byte
	for shift := 24; shift >= 0; shift -= 8 {
		if b := byte(id >> uint(shift)); b != 0 || len(out) > 0 {
			out = append(out, b)
		}
	}
	b := concat(body...)
	n := uint64(len(b))
	out = append(out, 0x01, byte(n>>48), byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(out, b...)
}

// A Matroska head with an HEVC video track and an English audio track (without which the parser rightly
// reports no tracks). `mapping` is the video's BlockAdditionMapping, or nil.
func mkvWithVideo(private []byte, mapping []byte) []byte {
	video := [][]byte{
		ebml(idTrackType, []byte{trackTypeVideo}),
		ebml(idCodecID, []byte("V_MPEGH/ISO/HEVC")),
		ebml(idCodecPrivate, private),
	}
	if mapping != nil {
		video = append(video, mapping)
	}
	audio := ebml(idTrackEntry, ebml(idTrackType, []byte{trackTypeAudio}), ebml(idLanguage, []byte("eng")))
	return ebml(idSegment, ebml(idTracks, ebml(idTrackEntry, video...), audio))
}

func TestParseMatroska_dolbyVisionBlockAdditionMapping(t *testing.T) {
	for _, c := range []struct {
		name    string
		mapping []byte
		dv      bool
		profile int
	}{
		{"dvvC with a profile 8.1 record", ebml(idBlockAdditionMapping,
			ebml(idBlockAddIDType, []byte("dvvC")), ebml(idBlockAddIDExtraData, doviP81)), true, 8},
		{"dvcC with a profile 7 record", ebml(idBlockAdditionMapping,
			ebml(idBlockAddIDType, []byte("dvcC")), ebml(idBlockAddIDExtraData, doviP7)), true, 7},
		// The tag alone still says Dolby Vision is there; it just can't say which profile.
		{"dvcC with no record", ebml(idBlockAdditionMapping, ebml(idBlockAddIDType, []byte("dvcC"))), true, 0},
		{"a mapping that isn't Dolby Vision", ebml(idBlockAdditionMapping,
			ebml(idBlockAddIDType, []byte("itut")), ebml(idBlockAddIDExtraData, doviP81)), false, 0},
		{"no mapping", nil, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParseMatroskaTracks(mkvWithVideo(hvcC(10), c.mapping))
			if err != nil {
				t.Fatal(err)
			}
			if p.VideoCodec != "hevc" || p.BitDepth != 10 {
				t.Errorf("codec/depth = %s/%d, want hevc/10 from CodecPrivate", p.VideoCodec, p.BitDepth)
			}
			if p.DolbyVision != c.dv || p.DVProfile != c.profile {
				t.Errorf("dv=%v profile=%d, want dv=%v profile=%d", p.DolbyVision, p.DVProfile, c.dv, c.profile)
			}
		})
	}
}
