package scout

import (
	"encoding/json"
	"net/http"
	"strings"
)

// A browser's report of what it plays, as Den Web sends it to den-remux and den-remux passes on in
// X-Den-Playable (den-edge web/src/lib/playable.ts). With it a stream list is ranked for that browser rather
// than for the Apple TV: releases it takes as they are first, then those den-remux has to convert, and last
// those nothing here can make play. Without it the list is the TV's, as it always was.
//
// Levels are the codecs' own numbers: H.264 `level_idc` (51 is 5.1), HEVC `general_level_idc` (level × 30,
// so 150 is 5.0), AV1 `seq_level_idx` (12 is 5.0). 0 means none.
type ClientPlayable struct {
	H264            int  `json:"h264"`
	H264High10      int  `json:"h264High10"`
	HEVCMain        int  `json:"hevcMain"`
	HEVCMain10      int  `json:"hevcMain10"`
	HEVCHighTier    int  `json:"hevcHighTier"`
	HDR             bool `json:"hdr"`
	EAC3            bool `json:"eac3"`
	AACMultichannel bool `json:"aacMultichannel"`
	DolbyVision     struct {
		P5 bool `json:"p5"`
		P8 bool `json:"p8"`
	} `json:"dolbyVision"`
	AV1       int  `json:"av1"`
	AV1Main10 int  `json:"av1Main10"`
	AV1HDR    bool `json:"av1Hdr"`
	// FLAC in fMP4, and VP9 profile 0 and 2: what den-remux copies rather than converts or skips for a browser that
	// plays them. den-remux clears VP9 profile 2 for a session in Safari's own player, where 10-bit VP9 is unmeasured.
	FLAC        bool `json:"flac"`
	AAC71       bool `json:"aac71"`
	VP9         bool `json:"vp9"`
	VP9Profile2 bool `json:"vp9Profile2"`
}

const playableHeader = "X-Den-Playable"

// The report is about 250 bytes; anything much longer is not one.
const maxPlayableHeader = 2048

// clientPlayable reads the report off a request, and a key for the list ranked by it. nil and "" when there
// is none, or it doesn't parse: a list ranked for the TV is a worse answer for a browser, not a wrong one.
// The key is taken from the report re-encoded, so browsers that report the same thing share one list however
// their JSON was spelled.
func clientPlayable(r *http.Request) (*ClientPlayable, string) {
	raw := r.Header.Get(playableHeader)
	if raw == "" {
		return nil, ""
	}
	var p ClientPlayable
	if len(raw) > maxPlayableHeader || json.Unmarshal([]byte(raw), &p) != nil {
		logLimited("playable-header", "stream: an %s header that does not parse; ranking for the TV", playableHeader)
		return nil, ""
	}
	canonical, _ := json.Marshal(p)
	return &p, keyHash(string(canonical))
}

// What a release costs in rank, for this browser, by what den-remux will have to do for it to play.
//
// Converted audio costs about a source bonus: den-remux does it on the CPU as it goes, and the picture is the
// release's own, so a 4K release whose TrueHD becomes AAC still ranks above a 1080p one. Video nobody named
// costs the same, since den-remux's probe settles it when the file is opened.
//
// Converted video costs more than the widest spread quality alone produces (4860, see preferenceSink), so every
// release that plays as it is ranks above every one that has to be transcoded — one at a time on the box's GPU,
// at most 1080p, tone-mapped — while staying under the cached bonus, so a cached conversion still beats a
// download. A release that can't play here at all sinks below every one that can, and stays above junk.
const (
	audioConvertedCost = 300
	videoUnnamedCost   = 300
	videoConvertedCost = 6000
	neverPlaysCost     = 60000
)

// den-remux opens Matroska and MP4 only, and refuses these by name.
var remuxRefusedExt = []string{".avi", ".ts", ".m2ts", ".iso", ".wmv", ".mpg", ".mpeg", ".vob", ".webm"}

// cost applies den-remux's own reading of scout's attributes (src/scout.rs `remuxable`, src/session.rs `fit`,
// and its audio choice): what it copies, what it converts, what it won't open.
func (p *ClientPlayable) cost(s RawStream, a StreamAttributes) int {
	if a.ThreeD || !remuxOpens(s) {
		return neverPlaysCost
	}
	cost := 0
	// Most Profile 5 has no base layer, but a malformed class has a P5 container record over a regular HDR base.
	// Keep proven P5 near the end so den-remux can inspect the HEVC VUI/RPU and either refuse genuine P5 or strip
	// the false record. Treating it as impossible here prevented that definitive probe from ever running.
	if a.DVProfile == 5 && !p.DolbyVision.P5 {
		cost += videoConvertedCost * 2
	}
	// A profile 5 the name only suggests weighs like a transcode: most likely it won't play here, but the probe
	// has the last word, so it stays in the list.
	if a.DVProfile == 0 && a.DVProfileGuess == 5 && !p.DolbyVision.P5 {
		cost += videoConvertedCost
	}
	uhd := deref(a.Resolution) == "2160p"
	tenBit := a.BitDepth >= 10
	// beyond answers whether the video is past what the decoder takes: by the level the probe read, else by
	// the level a 3840 × 2160 picture needs at least.
	beyond := func(most, uhdLevel int) bool {
		if a.VideoLevel > 0 {
			return a.VideoLevel > most
		}
		return uhd && most < uhdLevel
	}
	switch deref(a.Codec) {
	case "h264":
		// A 10 the name inferred from "HDR" is no evidence of High 10: H.264 releases are not HDR.
		most := p.H264
		if tenBit && (a.Probed || !a.HDR) {
			most = p.H264High10
		}
		// Nothing on the box makes H.264 smaller.
		if most == 0 || beyond(most, 51) {
			return neverPlaysCost
		}
	case "hevc":
		most := p.HEVCMain10
		if !tenBit {
			most = max(most, p.HEVCMain)
		}
		if a.HighTier {
			most = p.HEVCHighTier
		}
		if most == 0 || beyond(most, 150) || (a.HDR && !p.HDR) {
			cost += videoConvertedCost
		}
	case "av1":
		most := p.AV1Main10
		if !tenBit {
			most = max(most, p.AV1)
		}
		// Nothing converts AV1, and a browser is only asked about its Main tier.
		if most == 0 || a.HighTier || beyond(most, 12) || (a.HDR && !p.AV1HDR) {
			return neverPlaysCost
		}
	case "vp9":
		// Nothing converts VP9 either. Profile 2 is its 10-bit profile.
		takes := p.VP9
		if tenBit {
			takes = p.VP9Profile2
		}
		if !takes {
			return neverPlaysCost
		}
	case "":
		cost += videoUnnamedCost
	default:
		// MPEG-4 Part 2, VC-1, MPEG-2: den-remux converts none of them.
		return neverPlaysCost
	}
	switch deref(a.AudioCodec) {
	case "aac":
	case "eac3", "ac3":
		if !p.EAC3 {
			cost += audioConvertedCost
		}
	case "flac":
		if !p.FLAC {
			cost += audioConvertedCost
		}
	default:
		cost += audioConvertedCost
	}
	return cost
}

func remuxOpens(s RawStream) bool {
	switch containerOf(s) {
	case "avi", "webm":
		return false
	}
	name := strings.ToLower(s.Title)
	for _, ext := range remuxRefusedExt {
		if strings.HasSuffix(name, ext) {
			return false
		}
	}
	return true
}
