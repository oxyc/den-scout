package scout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Which audio and subtitle languages a release actually contains — read from the file itself rather than
// guessed from its name.
//
// The addon can only report what the indexer's title says, and titles routinely say nothing: a release
// offered for a Swedish series turned out to carry Italian audio and no subtitles at all, which no badge
// could have warned about. The container knows, and for Matroska it says so within the first megabyte, so
// one ranged request answers it exactly.
type Probe struct {
	// Which container this came from, and the video codec it declares. The codec is the reason AVI is
	// parsed at all: an XviD rip has no hardware decoder on an Apple TV, and that costs the viewer the
	// scrubber and the transport, not merely some quality.
	Container  string   `json:"container,omitempty"`
	VideoCodec string   `json:"videoCodec,omitempty"`
	Audio      []string `json:"audioLanguages"`
	Subtitles  []string `json:"subtitleLanguages"`
	// How many tracks carried no language at all. A release can have audio whose language nobody wrote
	// down, and "2 untagged audio tracks" is a different, more useful statement than "no audio".
	UntaggedAudio     int `json:"untaggedAudioTracks"`
	UntaggedSubtitles int `json:"untaggedSubtitleTracks"`
}

// Matroska EBML ids. Only the handful needed to walk Tracks -> TrackEntry -> the descriptive children.
const (
	idSegment      = 0x18538067
	idTracks       = 0x1654AE6B
	idTrackEntry   = 0xAE
	idTrackType    = 0x83
	idLanguage     = 0x22B59C // ISO 639-2, the historical field; defaults to "eng" when absent
	idLanguageBCP  = 0x22B59D // BCP-47, preferred when present
	trackTypeAudio = 2
	trackTypeSub   = 17
)

// probeBytes is how much of the file to read. Matroska writes SeekHead/Info/Tracks up front, and the real
// sample measured 5 KB to the end of Tracks — a megabyte is generous cover for muxers that pad, while
// staying small enough that probing is cheap against a metered debrid account.
const probeBytes = 1 << 20

// ErrNoTracks means the head carried no Tracks element: an MP4 with its moov at the end, a container this
// doesn't parse, or a truncated read. The caller keeps whatever the title told it rather than claiming
// something false.
var ErrNoTracks = errors.New("no track information in the file head")

// ProbeTracks reads the first megabyte of a resolved link and reports its audio/subtitle languages.
func ProbeTracks(ctx context.Context, client *http.Client, url string) (Probe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Probe{}, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))
	resp, err := client.Do(req)
	if err != nil {
		return Probe{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return Probe{}, fmt.Errorf("probe: unexpected status %d", resp.StatusCode)
	}
	// Bounded regardless of what the server does with the Range header — a 200 means the whole file is
	// coming, and reading all of a 20 GB remux to find a 5 KB header would be its own outage.
	head, err := io.ReadAll(io.LimitReader(resp.Body, probeBytes))
	if err != nil {
		return Probe{}, err
	}
	return ParseHead(head)
}

// ParseHead reads whatever the container will tell us: languages where the format records them, and the
// video codec where it declares one. Dispatches on the file's own magic rather than on a filename, which
// lies often enough that it was the reason for probing in the first place.
//
// Deliberately three small parsers instead of ffmpeg. Everything needed here — track types, languages,
// codec — sits in structures a few hundred lines can read, and the alternative is a 30-80 MB binary in a
// distroless image plus ffmpeg's parser exposed to untrusted remote bytes. If richer facts are ever wanted
// (channel counts, HDR metadata), ffprobe belongs BEHIND this as a fallback, not in front of it.
func ParseHead(head []byte) (Probe, error) {
	if p, ok := parseAVI(head); ok {
		return p, nil
	}
	if p, ok := parseMP4(head); ok {
		return p, nil
	}
	return ParseMatroskaTracks(head)
}

// ParseMatroskaTracks extracts the languages from a Matroska head. Pure, so the parsing is testable
// without a network or a whole file.
func ParseMatroskaTracks(head []byte) (Probe, error) {
	payload, ok := findTracksPayload(head)
	if !ok {
		return Probe{}, ErrNoTracks
	}
	out := Probe{Container: "matroska"}
	for pos := 0; pos < len(payload); {
		id, idLen := readID(payload[pos:])
		if idLen == 0 {
			break
		}
		body, next, ok := childPayload(payload, pos+idLen)
		if !ok {
			break
		}
		if id == idTrackEntry {
			kind, lang := parseTrackEntry(body)
			switch {
			case lang == "":
			case kind == trackTypeAudio && lang == "und":
				out.UntaggedAudio++
			case kind == trackTypeSub && lang == "und":
				out.UntaggedSubtitles++
			case kind == trackTypeAudio:
				out.Audio = appendUnique(out.Audio, lang)
			case kind == trackTypeSub:
				out.Subtitles = appendUnique(out.Subtitles, lang)
			}
		}
		pos = next
	}
	if len(out.Audio) == 0 && len(out.Subtitles) == 0 && out.UntaggedAudio == 0 && out.UntaggedSubtitles == 0 {
		return Probe{}, ErrNoTracks
	}
	return out, nil
}

// findTracksPayload walks the container to Tracks: top level for Segment, then Segment's children.
//
// Deliberately a WALK and not a search for the id. Matroska's SeekHead stores the ids of the elements it
// points at as DATA, so a file carries the four bytes of the Tracks id long before Tracks itself — the
// first version scanned, matched the pointer at offset 83, and parsed the seek table as if it were a track
// list. The real element was at 316.
func findTracksPayload(head []byte) ([]byte, bool) {
	for pos := 0; pos < len(head); {
		id, idLen := readID(head[pos:])
		if idLen == 0 {
			return nil, false
		}
		size, sizeLen, unknown, ok := readSizeAt(head, pos+idLen)
		if !ok {
			return nil, false
		}
		body := pos + idLen + sizeLen
		if id == idSegment {
			// Segment routinely declares an unknown size (a muxer writing a stream it hasn't finished),
			// in which case its children simply run to the end of what we read.
			end := len(head)
			if !unknown && body+size < end {
				end = body + size
			}
			return findTracksIn(head[body:end])
		}
		if unknown {
			return nil, false
		}
		pos = body + size
	}
	return nil, false
}

func findTracksIn(segment []byte) ([]byte, bool) {
	for pos := 0; pos < len(segment); {
		id, idLen := readID(segment[pos:])
		if idLen == 0 {
			return nil, false
		}
		size, sizeLen, unknown, ok := readSizeAt(segment, pos+idLen)
		if !ok || unknown {
			return nil, false
		}
		body := pos + idLen + sizeLen
		end := body + size
		if end > len(segment) || end < body {
			return nil, false
		}
		if id == idTracks {
			return segment[body:end], true
		}
		pos = end
	}
	return nil, false
}

// parseTrackEntry returns the track's type and language, or "und" when the file doesn't say.
//
// Matroska specifies that an absent Language means "eng", and the first version honoured that. On real
// files it is a trap: an untagged AC-3 track in a SWEDISH series would have been announced as English
// audio. In practice an absent tag means the muxer didn't fill it in, not that the audio is English — and
// a confident wrong language is worse for the viewer than an honest unknown.
func parseTrackEntry(entry []byte) (kind int, lang string) {
	lang = "und"
	for pos := 0; pos < len(entry); {
		id, idLen := readID(entry[pos:])
		if idLen == 0 {
			return 0, ""
		}
		body, next, ok := childPayload(entry, pos+idLen)
		if !ok {
			return 0, ""
		}
		switch id {
		case idTrackType:
			kind = int(uintFrom(body))
		case idLanguage:
			if s := cleanLang(body); s != "" {
				lang = s
			}
		case idLanguageBCP:
			// BCP-47 wins where both exist: it is the field a modern muxer fills in.
			if s := cleanLang(body); s != "" {
				lang = s
			}
		}
		pos = next
	}
	if kind != trackTypeAudio && kind != trackTypeSub {
		return kind, ""
	}
	return kind, lang
}

// cleanLang normalises a tag to a two-letter code where one exists.
//
// The same file can carry both ISO 639-2 ("swe") and BCP-47 ("sv") for the same track, and across the
// sweep the output mixed the two — "eng" for one release and "en" for the next, which no consumer can
// group or match on. Everything lands on ISO 639-1 where a mapping exists; anything else passes through
// as given rather than being dropped.
func cleanLang(b []byte) string {
	s := strings.ToLower(strings.TrimSpace(strings.TrimRight(string(b), "\x00")))
	if s == "und" || s == "" {
		return ""
	}
	// "pt-BR" and "pt" are the same shelf for a viewer choosing a track.
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	if two, ok := iso639[s]; ok {
		return two
	}
	return s
}

// ISO 639-2 (bibliographic AND terminological — muxers emit both) to 639-1, for the languages that
// actually turn up on releases. Unlisted codes pass through unchanged.
var iso639 = map[string]string{
	"eng": "en", "swe": "sv", "nor": "no", "nob": "no", "dan": "da", "fin": "fi", "isl": "is", "ice": "is",
	"ger": "de", "deu": "de", "fre": "fr", "fra": "fr", "spa": "es", "ita": "it", "por": "pt", "dut": "nl",
	"nld": "nl", "pol": "pl", "rus": "ru", "ukr": "uk", "cze": "cs", "ces": "cs", "slo": "sk", "slk": "sk",
	"hun": "hu", "rum": "ro", "ron": "ro", "gre": "el", "ell": "el", "tur": "tr", "ara": "ar", "heb": "he",
	"hin": "hi", "jpn": "ja", "kor": "ko", "chi": "zh", "zho": "zh", "cmn": "zh", "yue": "zh", "tha": "th",
	"vie": "vi", "ind": "id", "may": "ms", "msa": "ms", "fil": "fil", "hrv": "hr", "srp": "sr", "bul": "bg",
	"est": "et", "lav": "lv", "lit": "lt", "cat": "ca", "baq": "eu", "glg": "gl", "per": "fa", "fas": "fa",
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// indexOfID finds a 4-byte element id. Scanning beats walking the tree here: Segment routinely declares an
// unknown size, and the elements before Tracks vary by muxer, so a walk has more ways to go wrong than a
// search followed by a validating parse.
func indexOfID(buf []byte, id uint32) int {
	want := []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	for i := 0; i+4 <= len(buf); i++ {
		if buf[i] == want[0] && buf[i+1] == want[1] && buf[i+2] == want[2] && buf[i+3] == want[3] {
			return i
		}
	}
	return -1
}

// readID reads a variable-length element id, returning it and its byte length.
func readID(buf []byte) (uint32, int) {
	if len(buf) == 0 {
		return 0, 0
	}
	n := leadingLength(buf[0])
	if n == 0 || n > 4 || len(buf) < n {
		return 0, 0
	}
	var id uint32
	for i := 0; i < n; i++ {
		id = id<<8 | uint32(buf[i])
	}
	return id, n
}

// elementPayload reads a size at p and returns the bytes it covers.
func elementPayload(buf []byte, p int) ([]byte, bool) {
	size, n, ok := readSize(buf, p)
	if !ok {
		return nil, false
	}
	end := p + n + size
	if size < 0 || end > len(buf) || end < 0 {
		return nil, false
	}
	return buf[p+n : end], true
}

// childPayload reads a size at p and returns the payload plus the offset just past it.
func childPayload(buf []byte, p int) ([]byte, int, bool) {
	size, n, ok := readSize(buf, p)
	if !ok {
		return nil, 0, false
	}
	end := p + n + size
	if size < 0 || end > len(buf) || end < 0 {
		return nil, 0, false
	}
	return buf[p+n : end], end, true
}

// readSizeAt also reports EBML's unknown-size marker (all value bits set), which a Segment commonly uses.
func readSizeAt(buf []byte, p int) (size int, width int, unknown bool, ok bool) {
	if p >= len(buf) {
		return 0, 0, false, false
	}
	n := leadingLength(buf[p])
	if n == 0 || p+n > len(buf) {
		return 0, 0, false, false
	}
	v := int(buf[p] & (0xFF >> uint(n)))
	allOnes := int(buf[p]&(0xFF>>uint(n))) == int(0xFF>>uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | int(buf[p+i])
		allOnes = allOnes && buf[p+i] == 0xFF
	}
	return v, n, allOnes, true
}

func readSize(buf []byte, p int) (size int, width int, ok bool) {
	if p >= len(buf) {
		return 0, 0, false
	}
	n := leadingLength(buf[p])
	if n == 0 || p+n > len(buf) {
		return 0, 0, false
	}
	v := int(buf[p] & (0xFF >> uint(n)))
	for i := 1; i < n; i++ {
		v = v<<8 | int(buf[p+i])
	}
	return v, n, true
}

// leadingLength is the EBML width marker: the number of leading zero bits before the first 1, plus one.
func leadingLength(b byte) int {
	for i := 0; i < 8; i++ {
		if b&(0x80>>uint(i)) != 0 {
			return i + 1
		}
	}
	return 0
}

func uintFrom(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}
