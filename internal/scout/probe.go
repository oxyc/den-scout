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
type Tracks struct {
	Audio     []string `json:"audioLanguages"`
	Subtitles []string `json:"subtitleLanguages"`
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
func ProbeTracks(ctx context.Context, client *http.Client, url string) (Tracks, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Tracks{}, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", probeBytes-1))
	resp, err := client.Do(req)
	if err != nil {
		return Tracks{}, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return Tracks{}, fmt.Errorf("probe: unexpected status %d", resp.StatusCode)
	}
	// Bounded regardless of what the server does with the Range header — a 200 means the whole file is
	// coming, and reading all of a 20 GB remux to find a 5 KB header would be its own outage.
	head, err := io.ReadAll(io.LimitReader(resp.Body, probeBytes))
	if err != nil {
		return Tracks{}, err
	}
	return ParseMatroskaTracks(head)
}

// ParseMatroskaTracks extracts the languages from a Matroska head. Pure, so the parsing is testable
// without a network or a whole file.
func ParseMatroskaTracks(head []byte) (Tracks, error) {
	payload, ok := findTracksPayload(head)
	if !ok {
		return Tracks{}, ErrNoTracks
	}
	var out Tracks
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
			if kind, lang := parseTrackEntry(body); lang != "" {
				switch kind {
				case trackTypeAudio:
					out.Audio = appendUnique(out.Audio, lang)
				case trackTypeSub:
					out.Subtitles = appendUnique(out.Subtitles, lang)
				}
			}
		}
		pos = next
	}
	if len(out.Audio) == 0 && len(out.Subtitles) == 0 {
		return Tracks{}, ErrNoTracks
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

// parseTrackEntry returns the track's type and language. Matroska omits Language when it is "eng", so an
// entry with no language field is English by specification, not unknown.
func parseTrackEntry(entry []byte) (kind int, lang string) {
	lang = "eng"
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

func cleanLang(b []byte) string {
	s := strings.ToLower(strings.TrimSpace(strings.TrimRight(string(b), "\x00")))
	if s == "und" || s == "" {
		return ""
	}
	// "pt-BR" and "pt" are the same shelf for a viewer choosing a track.
	if i := strings.IndexAny(s, "-_"); i > 0 {
		s = s[:i]
	}
	return s
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
