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
	// `audioCodec` is the normalized family ("eac3","ac3","truehd","dts","dtshd","dtshdma","dtsx","flac",
	// "aac","opus","mp3"),
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
	// Whether the debrid already holds this release. A POINTER, because there are three answers and the
	// third one matters: it holds it, it does not, or nobody could ask. When the cache check failed a flat
	// `false` went out for every release — a definite claim — while the same response's
	// `X-Den-Degraded: cache-check` header said we did not know. The client's field is optional and
	// reads nil as "unknown", so the lie was believed: every release counted as needing a download, and
	// the app queued a real fetch for releases the debrid already held, during the minute the debrid was
	// already refusing requests. Omitted rather than guessed.
	Cached *bool `json:"cached,omitempty"`
	// Read from the file itself rather than its title, when the release was probed. Absent means it
	// wasn't — which the client must not read as "has none": an unprobed release is unknown, not empty.
	AudioLanguages    []string `json:"audioLanguages,omitempty"`
	SubtitleLanguages []string `json:"subtitleLanguages,omitempty"`
	UntaggedAudio     int      `json:"untaggedAudioTracks,omitempty"`
	Probed            bool     `json:"probed,omitempty"`
	Label             string   `json:"label"`
	// Video bit depth (8, 10) and Dolby Vision profile (5, 7, 8, …). 0 means nobody has read it, and is
	// omitted: an unprobed "DV" release has a profile we don't know, not no profile. The title supplies 10
	// for "10bit"/"Hi10P" and for any HDR or Dolby Vision release (both are 10-bit by definition); the
	// probe replaces that with what the codec's configuration record says.
	BitDepth  int `json:"bitDepth,omitempty"`
	DVProfile int `json:"dvProfile,omitempty"`
	// Whether a browser plays the file as it is, and what den-remux has to do before one can. See webHint.
	Web WebHint `json:"web"`
}

// WebHint is the answer webHint computes. `direct` is never null, so a client can test membership
// without a nil check; an empty list is a real answer ("no browser, as it stands").
type WebHint struct {
	Direct []string `json:"direct"`
	Remux  string   `json:"remux"`
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
	// A digit or a boundary after "aac" ("AAC2.0", "AAC5 1", "HE-AAC"), so a name like "Aachen" isn't audio.
	reAAC = mustRE2(`\baac(?:[0-9]|\b)`)
	// Opus counts only once the technical half of the name has begun: "Mr.Hollands.Opus.1995.1080p" and
	// "Opus.2025.1080p.WEB-DL" are film titles, and a bare \bopus\b called both of them Opus audio.
	reOpus   = mustRE2(`(?:\d{3,4}p|x26[45]|h\.?26[45]|hevc|av1|10.?bits?|web-?dl|webrip|bluray)\b.*\bopus\b`)
	reMP3    = mustRE2(`\bmp3\b`)
	reHi10   = mustRE2(`\bhi10p?\b`)
	reTenBit = mustRE2(`\b10[ .\-_]?bits?\b|\bhi10p?\b|\bmain ?10\b`)
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

// detectCodec reads the codec off the release title. Its spellings must match the ones the probe
// produces, because withProbe overwrites this field rather than merging with it.
//
// This is the "eng"/"en" problem cleanLang exists to fix, one field over, and it was live: the title
// path answered "avc" where all three container parsers answer "h264" for the same codec. Probing is
// asynchronous, so a viewer got "avc" in the first /stream response for a release and "h264" in the
// next — one codec, two spellings, which no client can badge or group on. h264 is the spelling that
// wins because three parsers already produce it and this was the only place that did not.
func detectCodec(t string) string {
	switch {
	case reAV1.match(t):
		return "av1"
	case reHEVC.match(t):
		return "hevc"
	// Hi10P is H.264's High 10 profile by name, and anime releases routinely give it instead of "x264".
	case reAVC.match(t) || reHi10.match(t):
		return "h264"
	}
	return ""
}

// detectBitDepth returns 10 when the title says so ("10bit", "10-bit", "Hi10P", "Main10") or the release
// is HDR or Dolby Vision, which are 10-bit by definition. Otherwise 0: a title that says nothing is not
// evidence of 8-bit.
func detectBitDepth(t string, hdr bool) int {
	if hdr || reTenBit.match(t) {
		return 10
	}
	return 0
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
//
// The browser codecs (aac, opus, mp3) come LAST. A release naming DD+ and AAC carries DD+ as its main
// track and AAC as the stereo companion, so it is a DD+ release.
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
	case reAAC.match(t):
		return "aac"
	case reOpus.match(t):
		return "opus"
	case reMP3.match(t):
		return "mp3"
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
	hdr := dolbyVision || reHDRExtra.match(t)
	attrs := StreamAttributes{
		Resolution:    strPtr(detectResolutionLower(t)),
		Source:        strPtr(detectSourceAttr(t)),
		Codec:         strPtr(detectCodec(t)),
		HDR:           hdr,
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
		Cached:        cachedClaim(s),
		Label:         cleanLabelLower(t, s), // reuse the title we already lowercased
		BitDepth:      detectBitDepth(t, hdr),
	}
	attrs = withProbe(attrs, s.Probe)
	attrs.Web = webHint(containerOf(s), attrs)
	return attrs
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
	if p.BitDepth != 0 {
		attrs.BitDepth = p.BitDepth
	}
	if p.DVProfile != 0 {
		attrs.DVProfile = p.DVProfile
	}
	return attrs
}

// containerOf names the file's container: the probe's when the file was read, else the release name's
// extension, else "". notWebReady and the web hint both ask this, so they can't disagree about a file.
func containerOf(s RawStream) string {
	if s.Probe != nil && s.Probe.Container != "" {
		return s.Probe.Container
	}
	t := strings.ToLower(s.Title)
	switch {
	case strings.HasSuffix(t, ".mp4"), strings.HasSuffix(t, ".m4v"):
		return "mp4"
	case strings.HasSuffix(t, ".mkv"):
		return "matroska"
	case strings.HasSuffix(t, ".webm"):
		return "webm"
	case strings.HasSuffix(t, ".avi"):
		return "avi"
	}
	return ""
}

// webHint answers two questions for a browser player, from the container, the video codec and bit depth,
// the Dolby Vision profile and the audio codec. The source of truth is the browser table in oxyc/den#11 §B.
//
// `direct` lists the browsers that play the file untouched from a plain <video src>:
//
//	               Safari (iOS, macOS)                 Chrome (desktop, Android)
//	container      MP4 only                            MP4, WebM, Matroska
//	video          H.264 8-bit, HEVC                   H.264 incl. 10-bit, HEVC*, AV1, VP9
//	Dolby Vision   profiles 5 and 8, not 7             not profile 5 (see dvNoFallback)
//	audio          AAC, MP3, FLAC, Opus                AAC, MP3, FLAC, Opus
//	never          AC-3/E-AC-3 (inside HLS only),      AC-3, E-AC-3, DTS, TrueHD
//	               DTS, TrueHD, AV1, VP9, MPEG-4 ASP
//
// * Chrome decodes HEVC only with a hardware decoder. It is listed because most current machines have
// one, but a client must still expect it to fail. Safari's AV1 is hardware-only too, on few enough
// devices that it is left out. Safari also needs HEVC in MP4 tagged `hvc1`, and fails on `hev1`; the
// probe does not record the tag.
//
// Anything unknown (container, codec, audio) is not direct. The answer errs toward "no".
// This is about the FILE: notWebReady also checks that the URL is https.
//
// `remux` says what den-remux must do for Safari to play its fMP4 HLS output. Safari is the target
// because it is the strictest. It decodes no video that Chrome can't, except Dolby Vision profile 5
// (check `dvProfile`) and HEVC on a machine with no hardware decoder. And den-remux's AAC is the one
// audio codec both browsers play inside HLS. So the answer holds for Chrome too, apart from those two.
//
//	"copy"   video and audio both copy; only the container changes (MKV → fMP4)
//	"audio"  the video copies, and the audio is re-encoded to AAC. That covers DD+, DTS, TrueHD, FLAC,
//	         Opus, MP3, and audio the title doesn't name.
//	"video"  Safari can't decode the video (H.264 10-bit/Hi10P, MPEG-4 ASP/XviD, VP9, AV1). Only a
//	         video re-encode would help, and den-remux doesn't do that.
//	"none"   the video codec is unknown, so there is nothing to answer from
//
// Profile 7 counts as its HDR10 base layer. Safari has no profile 7, so den-remux has to serve the
// stream without its Dolby Vision configuration for Safari to fall back to that base.
func webHint(container string, a StreamAttributes) WebHint {
	codec, audio := deref(a.Codec), deref(a.AudioCodec)
	h := WebHint{Direct: []string{}, Remux: remuxNeed(codec, audio, a.BitDepth)}
	if container == "mp4" && safariDecodes(codec, a.BitDepth) && a.DVProfile != 7 && webAudio[audio] {
		h.Direct = append(h.Direct, "safari")
	}
	chromeContainer := container == "mp4" || container == "matroska" || container == "webm"
	if chromeContainer && chromeDecodes(codec) && !dvNoFallback(a) && webAudio[audio] {
		h.Direct = append(h.Direct, "chrome")
	}
	return h
}

// The audio both browsers play in a progressive file. AC-3/E-AC-3 are left out: Safari plays them only
// inside HLS, and Chrome not at all.
var webAudio = map[string]bool{"aac": true, "mp3": true, "flac": true, "opus": true}

func remuxNeed(codec, audio string, bitDepth int) string {
	switch {
	case codec == "":
		return "none"
	case !safariDecodes(codec, bitDepth):
		return "video"
	case audio == "aac":
		return "copy"
	}
	return "audio"
}

// safariDecodes accepts HEVC at any depth and H.264 up to 8-bit. An unknown depth counts as 8, because
// 10-bit AVC is almost only anime Hi10P, and those releases say so in the name.
func safariDecodes(codec string, bitDepth int) bool {
	return codec == "hevc" || (codec == "h264" && bitDepth <= 8)
}

func chromeDecodes(codec string) bool {
	switch codec {
	case "h264", "hevc", "av1", "vp9":
		return true
	}
	return false
}

// dvNoFallback reports Dolby Vision with no base layer a non-DV player can show. That is profile 5,
// whose IPT picture comes out green and purple without DV processing. A profile nobody has read counts
// as 5, unless the title also names an HDR10-family format, as hybrid "DV HDR10" releases (profiles 7
// and 8) do.
func dvNoFallback(a StreamAttributes) bool {
	if !a.DolbyVision {
		return false
	}
	if a.DVProfile != 0 {
		return a.DVProfile == 5
	}
	return a.HDRFormat == nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// cachedClaim reports cachedness only when it was actually observed. A failed cache check leaves
// `Cached` at its zero value, and sending that as `false` is a claim nobody made.
func cachedClaim(s RawStream) *bool {
	if !s.CacheKnown {
		return nil
	}
	cached := s.Cached
	return &cached
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
