package scout

import (
	"encoding/binary"
	"strings"
)

// AVI carries no language metadata at all — the format predates the idea — so this reads only what it
// does know: the codec. That still matters, because the fourcc is how an XviD rip announces itself, and
// those decode in software on an Apple TV: no hardware decoder, no native transport, no scrubber.
func parseAVI(head []byte) (Probe, bool) {
	if len(head) < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "AVI " {
		return Probe{}, false
	}
	p := Probe{Container: "avi"}
	var lastKind string
	var walk func(buf []byte, start, end int)
	walk = func(buf []byte, start, end int) {
		i := start
		for i+8 <= end && i+8 <= len(buf) {
			id := string(buf[i : i+4])
			size := int(binary.LittleEndian.Uint32(buf[i+4 : i+8]))
			body := i + 8
			if size < 0 || body > len(buf) {
				return
			}
			switch id {
			case "LIST", "RIFF":
				stop := body + 4 + size
				if stop > len(buf) {
					stop = len(buf)
				}
				walk(buf, body+4, stop)
			case "strf":
				// Follows the strh it belongs to; for audio it is a WAVEFORMATEX whose second field is
				// the channel count.
				// Richest wins, as everywhere else — not the first stream listed.
				if lastKind == "auds" && body+4 <= len(buf) {
					if n := int(binary.LittleEndian.Uint16(buf[body+2 : body+4])); n > channelCount(p.AudioChannels) {
						p.AudioChannels = channelLayout(n)
					}
				}
			case "strh":
				if body+8 <= len(buf) {
					kind := string(buf[body : body+4])
					fourcc := strings.TrimRight(string(buf[body+4:body+8]), "\x00 ")
					switch kind {
					case "vids":
						if p.VideoCodec == "" {
							p.VideoCodec = codecFromFourCC(fourcc)
						}
					case "auds":
						// No language to read, but the track exists and the viewer should know it does.
						p.UntaggedAudio++
					}
					lastKind = kind
				}
			}
			i = body + size
			if size%2 == 1 {
				i++ // RIFF chunks are word-aligned
			}
		}
	}
	walk(head, 12, len(head))
	if p.VideoCodec == "" && p.UntaggedAudio == 0 {
		return Probe{}, false
	}
	return p, true
}

// codecFromFourCC maps the container's own label to the families Den ranks on. Unknown labels stay empty
// rather than guessing — an unrecognised fourcc is not evidence of anything.
func codecFromFourCC(cc string) string {
	switch strings.ToLower(cc) {
	case "xvid", "divx", "dx50", "divx5", "mp4v", "fmp4", "mp43":
		return "mpeg4"
	case "h264", "avc1", "x264", "davc":
		return "h264"
	case "hevc", "hvc1", "hev1", "x265":
		return "hevc"
	case "av01":
		return "av1"
	case "vp90", "vp09":
		return "vp9"
	}
	return ""
}
