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
		if kind == "soun" && p.AudioChannels == "" {
			if n := mp4AudioChannels(mdia); n > 0 {
				p.AudioChannels = channelLayout(n)
			}
		}
		if kind == "vide" {
			if mp4HasDolbyVision(mdia) {
				p.DolbyVision = true
			}
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
		switch {
		case size == 1: // 64-bit size follows the type
			if body+8 > len(buf) {
				return out
			}
			size = int(binary.BigEndian.Uint64(buf[body : body+8]))
			body += 8
		case size == 0: // runs to the end
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

// mp4HasDolbyVision looks for the DV configuration box, whose presence is the signal.
func mp4HasDolbyVision(mdia []byte) bool {
	entry, ok := firstSampleEntry(mdia)
	if !ok || len(entry) <= 78 {
		return false
	}
	for _, b := range boxes(entry[78:]) {
		if b.typ == "dvcC" || b.typ == "dvvC" {
			return true
		}
	}
	return false
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
