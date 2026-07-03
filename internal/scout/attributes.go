package scout

import "strings"

// Structured, pre-parsed display attributes (SCOUT-03, ported from src/attributes.ts). Emitted on each
// stream so the client renders badges without re-parsing titles. Nullable fields marshal as JSON null
// (no omitempty) to match the TS wire shape. Field order matches the TS object.
type StreamAttributes struct {
	Resolution  *string `json:"resolution"`
	Source      *string `json:"source"`
	Codec       *string `json:"codec"`
	HDR         bool    `json:"hdr"`
	DolbyVision bool    `json:"dolbyVision"`
	// The HDR10-family variant, so the client can badge it distinctly ("HDR10+", "HDR10", "HLG",
	// "HDR"), or null. Independent of DolbyVision (a stream can be DV *and* carry an HDR10 base) — the
	// client shows both. Note: Apple TV doesn't use HDR10+ dynamic metadata (it plays the HDR10 base),
	// so this is a label, not a reason to rank HDR10+ above Dolby Vision.
	HDRFormat   *string `json:"hdrFormat"`
	Audio       *string `json:"audio"`
	ThreeD      bool    `json:"threeD"`
	SizeBytes   *int    `json:"sizeBytes"`
	Seeders     *int    `json:"seeders"`
	Cached      bool    `json:"cached"`
	Label       string  `json:"label"`
}

var (
	reDoViAttr = mustRE2(`dolby vision|dolbyvision|dovi|\bdv\b`)
	reHDRExtra = mustRE2(`hdr10|\bhdr\b|\bhlg\b`)
	reHLG      = mustRE2(`\bhlg\b`)
	reHDR10any = mustRE2(`hdr10`) // matches "hdr10" and the "hdr10" in "hdr10+"; check reHDR10p first
	reHDRPlain = mustRE2(`\bhdr\b`)
	reHEVC     = mustRE2(`x265|h\.?265|hevc`)
	reAVC      = mustRE2(`x264|h\.?264|\bavc\b`)
	reDTSHDa   = mustRE2(`dts-hd|dts hd|dtshd`)
	reFLAC     = mustRE2(`flac|lpcm|pcm`)
)

func detectSourceAttr(t string) string {
	switch j := junkClassOf(t); j {
	case "cam", "telesync", "screener":
		return j
	}
	switch {
	case reRemux.match(t):
		return "remux"
	case reBluray.match(t) || reBrRip.match(t):
		return "bluray"
	case reWebDL.match(t):
		return "webdl"
	case reWebRip.match(t):
		return "webrip"
	case reWeb.match(t):
		return "web"
	case reHDTV.match(t):
		return "hdtv"
	case reDvdRip.match(t):
		return "dvdrip"
	}
	return ""
}

func detectCodec(t string) string {
	switch {
	case reAV1.match(t):
		return "av1"
	case reHEVC.match(t):
		return "hevc"
	case reAVC.match(t):
		return "avc"
	}
	return ""
}

// detectHDRFormat returns the HDR10-family label ("HDR10+", "HLG", "HDR10", "HDR") or "". Ordered so
// HDR10+ wins over a bare "hdr10" token and HLG over generic "hdr". Dolby Vision is reported separately
// (a stream can be both), so it's intentionally not returned here.
func detectHDRFormat(t string) string {
	switch {
	case reHDR10p.match(t):
		return "HDR10+"
	case reHLG.match(t):
		return "HLG"
	case reHDR10any.match(t):
		return "HDR10"
	case reHDRPlain.match(t):
		return "HDR"
	}
	return ""
}

func detectAudio(t string) string {
	switch {
	case reAtmos.match(t):
		return "Atmos"
	case reDTSX.match(t):
		return "DTS:X"
	case reTrueHD.match(t):
		return "TrueHD"
	case reDTSHDa.match(t):
		return "DTS-HD"
	case reFLAC.match(t):
		return "FLAC"
	case reEAC3.match(t):
		return "EAC3"
	case reDTS.match(t):
		return "DTS"
	case reAC3.match(t):
		return "AC3"
	}
	return ""
}

func streamAttributes(s RawStream) StreamAttributes {
	t := strings.ToLower(s.Title)
	dolbyVision := reDoViAttr.match(t)
	return StreamAttributes{
		Resolution:  strPtr(detectResolutionLower(t)),
		Source:      strPtr(detectSourceAttr(t)),
		Codec:       strPtr(detectCodec(t)),
		HDR:         dolbyVision || reHDRExtra.match(t),
		DolbyVision: dolbyVision,
		HDRFormat:   strPtr(detectHDRFormat(t)),
		Audio:       strPtr(detectAudio(t)),
		ThreeD:      re3D.match(t),
		SizeBytes:   s.SizeBytes,
		Seeders:     s.Seeders,
		Cached:      s.Cached,
		Label:       cleanLabelLower(t, s), // reuse the title we already lowercased
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
