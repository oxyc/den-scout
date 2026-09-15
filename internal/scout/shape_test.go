package scout

import (
	"encoding/binary"
	"testing"
)

// A visual sample entry with its width and height at offsets 24 and 26, and config boxes past the 78-byte header.
func sizedVideoEntry(fourCC string, width, height int, extra ...[]byte) []byte {
	head := make([]byte, 78)
	binary.BigEndian.PutUint16(head[24:26], uint16(width))
	binary.BigEndian.PutUint16(head[26:28], uint16(height))
	return mp4box(fourCC, append(head, concat(extra...)...))
}

// A video trak whose mdhd carries a timescale and whose stts times every sample by one delta.
func timedVideoTrak(timescale, samples, delta uint32, entry []byte) []byte {
	mdhdBody := make([]byte, 24)
	binary.BigEndian.PutUint32(mdhdBody[12:16], timescale)
	stts := make([]byte, 16)
	binary.BigEndian.PutUint32(stts[4:8], 1)
	binary.BigEndian.PutUint32(stts[8:12], samples)
	binary.BigEndian.PutUint32(stts[12:16], delta)
	mdia := mp4box("mdia", hdlr("vide"), mp4box("mdhd", mdhdBody),
		mp4box("minf", mp4box("stbl", stsd(entry), mp4box("stts", stts))))
	return mp4box("trak", mdia)
}

func nclx(transfer uint16) []byte {
	body := make([]byte, 11)
	copy(body, "nclx")
	binary.BigEndian.PutUint16(body[6:8], transfer)
	return mp4box("colr", body)
}

// hvcC with a tier flag and level: byte 1 packs profile space, tier and profile; byte 12 is the level.
func hvcCShape(profile byte, highTier bool, level byte) []byte {
	rec := hvcC(10)
	rec[1] = profile
	if highTier {
		rec[1] |= 0x20
	}
	rec[12] = level
	return rec
}

func TestParseMP4_videoShape(t *testing.T) {
	for _, c := range []struct {
		name    string
		trak    []byte
		want    Probe
		wantHDR string
	}{
		{"HEVC Main 10 high tier L5.1, PQ, 23.976 fps",
			timedVideoTrak(24000, 240, 1001, sizedVideoEntry("hvc1", 3840, 1608,
				mp4box("hvcC", hvcCShape(2, true, 153)), nclx(16))),
			Probe{VideoCodec: "hevc", VideoProfile: 2, VideoLevel: 153, HighTier: true, Width: 3840, Height: 1608,
				FrameRate: 23.976, BitDepth: 10}, "HDR10"},
		{"H.264 High L4.1, 25 fps",
			timedVideoTrak(25, 100, 1, sizedVideoEntry("avc1", 1920, 1080, mp4box("avcC", []byte{1, 100, 0, 41}))),
			Probe{VideoCodec: "h264", VideoProfile: 100, VideoLevel: 41, Width: 1920, Height: 1080, FrameRate: 25,
				BitDepth: 8}, ""},
		{"AV1 Main 10-bit level 5.0, HLG",
			timedVideoTrak(60000, 600, 1001, sizedVideoEntry("av01", 3840, 2160,
				mp4box("av1C", []byte{0x81, 0<<5 | 12, 0x40}), nclx(18))),
			Probe{VideoCodec: "av1", VideoProfile: 0, VideoLevel: 12, Width: 3840, Height: 2160, FrameRate: 59.94,
				BitDepth: 10}, "HLG"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ok := parseMP4(mp4File(c.trak))
			if !ok {
				t.Fatal("not parsed")
			}
			got := Probe{VideoCodec: p.VideoCodec, VideoProfile: p.VideoProfile, VideoLevel: p.VideoLevel,
				HighTier: p.HighTier, Width: p.Width, Height: p.Height, FrameRate: p.FrameRate, BitDepth: p.BitDepth}
			if got.VideoCodec != c.want.VideoCodec || got.VideoProfile != c.want.VideoProfile ||
				got.VideoLevel != c.want.VideoLevel || got.HighTier != c.want.HighTier || got.Width != c.want.Width ||
				got.Height != c.want.Height || got.FrameRate != c.want.FrameRate || got.BitDepth != c.want.BitDepth {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
			if p.HDRFormat != c.wantHDR {
				t.Errorf("hdrFormat = %q, want %q", p.HDRFormat, c.wantHDR)
			}
		})
	}
}

func TestParseMatroska_videoShape(t *testing.T) {
	video := ebml(idVideo,
		ebml(idPixelWidth, []byte{0x0F, 0x00}),
		ebml(idPixelHeight, []byte{0x06, 0x48}),
		ebml(idColour, ebml(idTransferCharacteristics, []byte{16})))
	entry := ebml(idTrackEntry,
		ebml(idTrackType, []byte{trackTypeVideo}),
		ebml(idCodecID, []byte("V_MPEGH/ISO/HEVC")),
		ebml(idCodecPrivate, hvcCShape(2, false, 150)),
		ebml(idDefaultDuration, []byte{0x02, 0x7C, 0x6D, 0x5D}), // 41708381 ns: 23.976 fps
		video)
	audio := ebml(idTrackEntry, ebml(idTrackType, []byte{trackTypeAudio}), ebml(idLanguage, []byte("eng")))
	p, err := ParseMatroskaTracks(ebml(idSegment, ebml(idTracks, entry, audio)))
	if err != nil {
		t.Fatal(err)
	}
	if p.VideoLevel != 150 || p.HighTier || p.Width != 3840 || p.Height != 1608 || p.FrameRate != 23.976 ||
		p.HDRFormat != "HDR10" {
		t.Errorf("got level %d high %v %dx%d %v fps %q", p.VideoLevel, p.HighTier, p.Width, p.Height, p.FrameRate,
			p.HDRFormat)
	}
}

// ffmpeg's own output (testdata/README.md), in MP4 and Matroska.
func TestParseHead_videoShapeFromRealFiles(t *testing.T) {
	for _, c := range []struct {
		file          string
		width, height int
		fps           float64
	}{
		{"hevc10.mp4", 64, 64, 5},
		{"hi10p.mkv", 64, 64, 5},
		{"sample.mp4", 320, 240, 15},
	} {
		t.Run(c.file, func(t *testing.T) {
			p, err := ParseHead(readFixture(t, c.file))
			if err != nil {
				t.Fatal(err)
			}
			if p.Width != c.width || p.Height != c.height || p.FrameRate != c.fps || p.VideoLevel == 0 {
				t.Errorf("got %dx%d at %v fps, level %d; want %dx%d at %v fps and a level", p.Width, p.Height,
					p.FrameRate, p.VideoLevel, c.width, c.height, c.fps)
			}
		})
	}
}

func TestGuessDVProfile(t *testing.T) {
	for title, want := range map[string]int{
		"Film.2021.2160p.UHD.BluRay.REMUX.DV.HDR10.HEVC.TrueHD.7.1": 7,
		"Film.2021.2160p.BluRay.DV.x265.10bit":                      8,
		"Film.2021.2160p.WEB-DL.DV.HDR10.HEVC.DDP5.1":               8,
		"Film.2021.2160p.HYBRID.DV.x265":                            8,
		"Film.2021.2160p.NF.WEB-DL.DV.HEVC.DDP5.1":                  5,
		"Film.2021.2160p.DV.HEVC":                                   0,
		"Film.2021.2160p.WEB-DL.HDR10.HEVC":                         0, // no Dolby Vision to guess about
	} {
		if got := streamAttributes(RawStream{Title: title}).DVProfileGuess; got != want {
			t.Errorf("guess(%q) = %d, want %d", title, got, want)
		}
	}
	probed := streamAttributes(RawStream{Title: "Film.2021.2160p.WEB-DL.DV.HEVC",
		Probe: &Probe{VideoCodec: "hevc", DolbyVision: true, DVProfile: 8}})
	if probed.DVProfileGuess != 0 || probed.DVProfile != 8 {
		t.Errorf("a probed profile must replace the guess: profile %d, guess %d", probed.DVProfile, probed.DVProfileGuess)
	}
}

func TestClientPlayable_costFromTheProbe(t *testing.T) {
	probed := func(title string, p Probe) (RawStream, StreamAttributes) {
		s := RawStream{Title: title, Probe: &p}
		return s, streamAttributes(s)
	}
	browser := aBrowser(func(p *ClientPlayable) { p.HEVCHighTier = 0 })
	for _, c := range []struct {
		name  string
		title string
		probe Probe
		want  int
	}{
		{"HEVC high tier to a browser without it is transcoded", "Film.2160p.REMUX.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "hevc", BitDepth: 10, VideoLevel: 153, HighTier: true, Width: 3840},
			videoConvertedCost},
		{"HEVC over the browser's level is transcoded", "Film.2160p.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "hevc", BitDepth: 10, VideoLevel: 183, Width: 7680},
			videoConvertedCost},
		{"H.264 over the browser's level can't play", "Film.1080p.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "h264", BitDepth: 8, VideoLevel: 52, Width: 1920},
			neverPlaysCost},
		{"H.264 within it plays as it is", "Film.1080p.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "h264", BitDepth: 8, VideoLevel: 41, Width: 1920}, 0},
		{"an HDR transfer the name never gave counts as HDR", "Film.1080p.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "hevc", BitDepth: 10, VideoLevel: 123, Width: 1920, HDRFormat: "HDR10"},
			0},
		{"VP9 profile 0 to a browser that takes none can't play", "Film.1080p.AAC.mkv",
			Probe{Container: "matroska", VideoCodec: "vp9", BitDepth: 8, Width: 1920}, neverPlaysCost},
		{"FLAC to a browser without it is converted", "Film.1080p.x264.FLAC.mkv",
			Probe{Container: "matroska", VideoCodec: "h264", BitDepth: 8, VideoLevel: 41, Width: 1920}, audioConvertedCost},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, a := probed(c.title, c.probe)
			if got := browser.cost(s, a); got != c.want {
				t.Errorf("cost = %d, want %d", got, c.want)
			}
		})
	}

	noHDR := aBrowser(func(p *ClientPlayable) { p.HDR = false })
	s, a := probed("Film.1080p.AAC.mkv", Probe{Container: "matroska", VideoCodec: "hevc", BitDepth: 10, VideoLevel: 123,
		Width: 1920, HDRFormat: "HDR10"})
	if got := noHDR.cost(s, a); got != videoConvertedCost {
		t.Errorf("HDR the probe found, to a browser without HDR: cost %d, want %d", got, videoConvertedCost)
	}

	copies := aBrowser(func(p *ClientPlayable) { p.FLAC, p.VP9 = true, true })
	s, a = probed("Film.1080p.FLAC.mkv", Probe{Container: "matroska", VideoCodec: "vp9", BitDepth: 8, Width: 1920})
	if got := copies.cost(s, a); got != 0 {
		t.Errorf("VP9 with FLAC to a browser that plays both: cost %d, want 0", got)
	}

	guessed := RawStream{Title: "Film.2160p.NF.WEB-DL.DV.HEVC.AAC.mkv"}
	if got := aBrowser(nil).cost(guessed, streamAttributes(guessed)); got != videoConvertedCost {
		t.Errorf("a web DV release guessed as profile 5: cost %d, want %d", got, videoConvertedCost)
	}
}
