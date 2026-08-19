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
	HDRFormat *string `json:"hdrFormat"`
	// Source-truth audio, so the client can compute what it will actually DELIVER (Den bridges TrueHD /
	// DTS / DTS-HD to EAC3 5.1 and only DD+/EAC3+JOC keeps real Atmos). `audio` is the display string;
	// `audioCodec` is the normalized family ("eac3","ac3","truehd","dts","dtshd","dtshdma","dtsx","flac"),
	// `audioChannels` the layout ("7.1"/"5.1"/"2.0"), `atmos` whether the SOURCE carries Atmos.
	Audio         *string `json:"audio"`
	AudioCodec    *string `json:"audioCodec"`
	AudioChannels *string `json:"audioChannels"`
	Atmos         bool    `json:"atmos"`
	// Burned-in (hardcoded) subtitles — korsub/HC. A real gotcha, so the client can surface it.
	HardcodedSubs bool `json:"hardcodedSubs"`
	ThreeD        bool `json:"threeD"`
	SizeBytes     *int `json:"sizeBytes"`
	Seeders       *int `json:"seeders"`
	Cached        bool `json:"cached"`
	// Read from the file itself rather than its title, when the release was probed. Absent means it
	// wasn't — which the client must not read as "has none": an unprobed release is unknown, not empty.
	AudioLanguages    []string `json:"audioLanguages,omitempty"`
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
	UntaggedAudio     int      `json:"untaggedAudioTracks,omitempty"`
	Probed            bool     `json:"probed,omitempty"`
	Label             string   `json:"label"`
}

var (
	reDoViAttr = mustRE2(`dolby vision|dolbyvision|dovi|\bdv\b`)
	reHDRExtra = mustRE2(`hdr10|\bhdr\b|\bhlg\b`)
	reHLG      = mustRE2(`\bhlg\b`)
	reHDR10any = mustRE2(`hdr10`) // matches "hdr10" and the "hdr10" in "hdr10+"; check reHDR10p first
	reHDRPlain = mustRE2(`\bhdr\b`)
	reCh71     = mustRE2(`7\.1|7 1|\b8ch\b`)
	reCh51     = mustRE2(`5\.1|5 1|\b6ch\b`)
	reCh20     = mustRE2(`2\.0|2 0|stereo|\b2ch\b`)
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

// detectAudioCodec returns the normalized source audio family (or ""). Most specific first, so
// "DTS-HD MA" isn't caught by the bare "dts" rule. The client uses this to know whether Den will
// stream-copy the audio (aac/ac3/eac3/flac) or bridge it to EAC3 5.1 (truehd/dts/dts-hd/dts:x).
func detectAudioCodec(t string) string {
	switch {
	case reDTSX.match(t):
		return "dtsx"
	case reTrueHD.match(t):
		return "truehd"
	case reDTSHDMA.match(t):
		return "dtshdma"
	case reDTSHDa.match(t):
		return "dtshd"
	case reFLAC.match(t):
		return "flac"
	case reEAC3.match(t):
		return "eac3"
	case reDTS.match(t):
		return "dts"
	case reAC3.match(t):
		return "ac3"
	}
	return ""
}

// detectChannels returns "7.1"/"5.1"/"2.0" or "" (e.g. "DDP5.1"/"DDP5 1" → "5.1").
func detectChannels(t string) string {
	switch {
	case reCh71.match(t):
		return "7.1"
	case reCh51.match(t):
		return "5.1"
	case reCh20.match(t):
		return "2.0"
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
	attrs := StreamAttributes{
		Resolution:    strPtr(detectResolutionLower(t)),
		Source:        strPtr(detectSourceAttr(t)),
		Codec:         strPtr(detectCodec(t)),
		HDR:           dolbyVision || reHDRExtra.match(t),
		DolbyVision:   dolbyVision,
		HDRFormat:     strPtr(detectHDRFormat(t)),
		Audio:         strPtr(detectAudio(t)),
		AudioCodec:    strPtr(detectAudioCodec(t)),
		AudioChannels: strPtr(detectChannels(t)),
		Atmos:         reAtmos.match(t),
		HardcodedSubs: reKorsubHC.match(t),
		ThreeD:        re3D.match(t),
		SizeBytes:     s.SizeBytes,
		Seeders:       s.Seeders,
		Cached:        s.Cached,
		Label:         cleanLabelLower(t, s), // reuse the title we already lowercased
	}
	return withProbe(attrs, s.Probe)
}

// withProbe lets the FILE override the title wherever it has something to say. The title is what an
// uploader typed; these are fields a muxer had to fill in — so codec and channel layout are corrected
// where they disagree, and languages are added, since no title states them reliably.
//
// Only ever overrides with a non-empty value: a probe that couldn't read a field leaves the title's guess
// standing rather than blanking it.
func withProbe(attrs StreamAttributes, p *Probe) StreamAttributes {
	if p == nil {
		return attrs
	}
	attrs.Probed = true
	attrs.AudioLanguages = p.Audio
	attrs.SubtitleLanguages = p.Subtitles
	attrs.UntaggedAudio = p.UntaggedAudio
	if p.VideoCodec != "" {
		attrs.Codec = strPtr(p.VideoCodec)
	}
	if p.AudioChannels != "" {
		attrs.AudioChannels = strPtr(p.AudioChannels)
	}
	if p.DolbyVision {
		attrs.DolbyVision = true
		attrs.HDR = true
	}
	return attrs
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
