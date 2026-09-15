package scout

import (
	"encoding/binary"
	"strings"
)

// ISO base media (MP4/MOV/M4V). Its metadata lives in `moov`, which a streaming-friendly muxer puts at
// the FRONT — but plenty of tools leave it at the end, where a one-megabyte head can't see it. That case
// reports nothing rather than guessing, and the caller keeps whatever the release title claimed.
func parseMP4(head []byte) (Probe, bool) {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return Probe{}, false
	}
	moov, ok := findBox(head, "moov")
	if !ok {
		return Probe{}, false
	}
	p := Probe{Container: "mp4"}
	for _, trak := range allBoxes(moov, "trak") {
		mdia, ok := findBox(trak, "mdia")
		if !ok {
			continue
		}
		kind := handlerKind(mdia)
		lang := mediaLanguage(mdia)
		if codec := sampleCodec(mdia); codec != "" && kind == "vide" && p.VideoCodec == "" {
			p.VideoCodec = codec
		}
		// The RICHEST audio track wins, not the first one listed — the rule probe.go states and readTrackFacts
		// already follows. First-wins reported "5.1" for a remux that lists 5.1 before its 7.1 track, and the
		// probe overrides the correctly-titled value, so the viewer is shown a worse fact than the title had.
		if kind == "soun" {
			if n := mp4AudioChannels(mdia); n > channelCount(p.AudioChannels) {
				p.AudioChannels = channelLayout(n)
			}
		}
		if kind == "vide" {
			mp4VideoConfig(mdia, &p)
		}
		switch kind {
		case "soun":
			if lang == "" {
				p.UntaggedAudio++
			} else {
				p.Audio = appendUnique(p.Audio, lang)
			}
		case "sbtl", "subt", "text", "clcp":
			if lang == "" {
				p.UntaggedSubtitles++
			} else {
				p.Subtitles = appendUnique(p.Subtitles, lang)
			}
		}
	}
	if p.VideoCodec == "" && len(p.Audio) == 0 && len(p.Subtitles) == 0 &&
		p.UntaggedAudio == 0 && p.UntaggedSubtitles == 0 {
		return Probe{}, false
	}
	return p, true
}

func handlerKind(mdia []byte) string {
	hdlr, ok := findBox(mdia, "hdlr")
	if !ok || len(hdlr) < 12 {
		return ""
	}
	return string(hdlr[8:12])
}

// mediaLanguage reads mdhd's packed language: three 5-bit values, each an ISO 639-2 letter offset from
// 0x60. "und" and the all-zero placeholder both mean nobody said.
func mediaLanguage(mdia []byte) string {
	mdhd, ok := findBox(mdia, "mdhd")
	if !ok || len(mdhd) < 24 {
		return ""
	}
	version := mdhd[0]
	off := 20 // v0: version+flags(4) + created(4) + modified(4) + timescale(4) + duration(4)
	if version == 1 {
		off = 32 // v1 widens created/modified/duration to 64-bit
	}
	if off+2 > len(mdhd) {
		return ""
	}
	packed := binary.BigEndian.Uint16(mdhd[off : off+2])
	var out [3]byte
	for i := 0; i < 3; i++ {
		out[2-i] = byte((packed>>(5*uint(i)))&0x1F) + 0x60
	}
	code := strings.ToLower(string(out[:]))
	for _, c := range code {
		if c < 'a' || c > 'z' {
			return ""
		}
	}
	return cleanLang([]byte(code))
}

func sampleCodec(mdia []byte) string {
	minf, ok := findBox(mdia, "minf")
	if !ok {
		return ""
	}
	stbl, ok := findBox(minf, "stbl")
	if !ok {
		return ""
	}
	stsd, ok := findBox(stbl, "stsd")
	if !ok || len(stsd) < 16 {
		return ""
	}
	// stsd: version+flags(4), entry count(4), then boxes whose type IS the codec.
	if len(stsd) < 12 {
		return ""
	}
	return codecFromFourCC(strings.TrimRight(string(stsd[12:16]), "\x00 "))
}

// findBox returns the payload of the first child box with this type.
func findBox(buf []byte, want string) ([]byte, bool) {
	for _, b := range boxes(buf) {
		if b.typ == want {
			return b.body, true
		}
	}
	return nil, false
}

func allBoxes(buf []byte, want string) [][]byte {
	var out [][]byte
	for _, b := range boxes(buf) {
		if b.typ == want {
			out = append(out, b.body)
		}
	}
	return out
}

type mp4Box struct {
	typ  string
	body []byte
}

func boxes(buf []byte) []mp4Box {
	var out []mp4Box
	for i := 0; i+8 <= len(buf); {
		size := int(binary.BigEndian.Uint32(buf[i : i+4]))
		typ := string(buf[i+4 : i+8])
		body := i + 8
		switch size {
		case 1: // 64-bit size follows the type
			if body+8 > len(buf) {
				return out
			}
			size = int(binary.BigEndian.Uint64(buf[body : body+8]))
			body += 8
		case 0: // runs to the end
			size = len(buf) - i
		}
		end := i + size
		if size <= 0 || end > len(buf) || body > end {
			return out
		}
		out = append(out, mp4Box{typ: typ, body: buf[body:end]})
		i = end
	}
	return out
}

// mp4AudioChannels reads the channel count from the audio sample entry: 6 bytes reserved, 2 data-reference
// index, 8 version/reserved, then channel count.
func mp4AudioChannels(mdia []byte) int {
	entry, ok := firstSampleEntry(mdia)
	if !ok || len(entry) < 18 {
		return 0
	}
	return int(binary.BigEndian.Uint16(entry[16:18]))
}

// mp4VideoConfig reads the configuration boxes after the video sample entry's fixed 78-byte header:
// avcC/hvcC for the bit depth, and dvcC/dvvC/dvwC for Dolby Vision, whose presence is the signal, and
// its profile. The first video track's depth wins, as its codec does.
func mp4VideoConfig(mdia []byte, p *Probe) {
	entry, ok := firstSampleEntry(mdia)
	if !ok || len(entry) <= 78 {
		return
	}
	// The first video track's shape, as its codec is.
	first := p.Width == 0 && p.VideoLevel == 0
	if first {
		// The visual sample entry's fixed header: 6 bytes reserved, 2 data-reference index, 16 pre-defined and
		// reserved, then width and height.
		p.Width, p.Height = int(binary.BigEndian.Uint16(entry[24:26])), int(binary.BigEndian.Uint16(entry[26:28]))
		p.FrameRate = mp4FrameRate(mdia)
	}
	for _, b := range boxes(entry[78:]) {
		switch b.typ {
		case "avcC", "hvcC", "av1C":
			codec := map[string]string{"avcC": "h264", "hvcC": "hevc", "av1C": "av1"}[b.typ]
			if p.BitDepth == 0 {
				p.BitDepth = bitDepthFromConfig(codec, b.body)
			}
			if first {
				p.VideoProfile, p.VideoLevel, p.HighTier = videoShape(codec, b.body)
			}
		case "colr":
			// An nclx colour box: its type, then colour primaries, transfer characteristics and matrix, 16 bits each.
			if first && len(b.body) >= 8 && string(b.body[:4]) == "nclx" {
				p.HDRFormat = hdrFromTransfer(int(binary.BigEndian.Uint16(b.body[6:8])))
			}
		case "dvcC", "dvvC", "dvwC":
			p.DolbyVision = true
			if p.DVProfile == 0 {
				p.DVProfile = doviProfile(b.body)
			}
		}
	}
}

// mp4FrameRate is frames per second over the whole track: stts's sample counts over their total duration, in
// mdhd's timescale. The first entry alone misreads a track whose opening samples are timed differently. 0 when
// either box is missing.
func mp4FrameRate(mdia []byte) float64 {
	mdhd, ok := findBox(mdia, "mdhd")
	if !ok || len(mdhd) < 24 {
		return 0
	}
	off := 12 // v0: version+flags(4) + created(4) + modified(4)
	if mdhd[0] == 1 {
		off = 20 // v1 widens created/modified to 64-bit
	}
	if off+4 > len(mdhd) {
		return 0
	}
	timescale := binary.BigEndian.Uint32(mdhd[off : off+4])
	minf, ok := findBox(mdia, "minf")
	if !ok {
		return 0
	}
	stbl, ok := findBox(minf, "stbl")
	if !ok {
		return 0
	}
	stts, ok := findBox(stbl, "stts")
	if !ok || len(stts) < 8 || timescale == 0 {
		return 0
	}
	// version+flags(4), entry count(4), then sample count and sample delta, 32 bits each, per entry.
	count := int(binary.BigEndian.Uint32(stts[4:8]))
	var samples, ticks uint64
	for i, at := 0, 8; i < count && at+8 <= len(stts); i, at = i+1, at+8 {
		n := uint64(binary.BigEndian.Uint32(stts[at : at+4]))
		samples += n
		ticks += n * uint64(binary.BigEndian.Uint32(stts[at+4:at+8]))
	}
	if ticks == 0 {
		return 0
	}
	return roundRate(float64(samples) * float64(timescale) / float64(ticks))
}

func firstSampleEntry(mdia []byte) ([]byte, bool) {
	minf, ok := findBox(mdia, "minf")
	if !ok {
		return nil, false
	}
	stbl, ok := findBox(minf, "stbl")
	if !ok {
		return nil, false
	}
	stsd, ok := findBox(stbl, "stsd")
	if !ok || len(stsd) < 8 {
		return nil, false
	}
	all := boxes(stsd[8:]) // skip version/flags + entry count
	if len(all) == 0 {
		return nil, false
	}
	return all[0].body, true
}
