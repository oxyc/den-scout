package scout

import (
	"encoding/binary"
	"os"
	"testing"
)

// The MP4 side of the probe is a byte parser over untrusted input: the head of a file served by a debrid
// link, which may be truncated, may be a different container wearing an .mp4 name, or may be hostile.
// Building the boxes here rather than shipping binary fixtures keeps each case's INTENT readable — the
// difference between a v0 and a v1 mdhd is one visible byte, not a hexdump.

// mp4box assembles size+type+payload. Sizes are 32-bit, which is what every box below needs.
func mp4box(typ string, payload ...[]byte) []byte {
	var body []byte
	for _, p := range payload {
		body = append(body, p...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

// hdlr — `handlerKind` reads the four bytes at payload offset 8.
func hdlr(kind string) []byte {
	p := make([]byte, 12)
	copy(p[8:12], kind)
	return mp4box("hdlr", p)
}

// mdhd v0 — the packed language sits at payload offset 20; v1 widens the timestamps and moves it to 32.
func mdhd(lang string, version byte) []byte {
	off := 20
	if version == 1 {
		off = 32
	}
	p := make([]byte, off+4)
	p[0] = version
	if lang != "" {
		var packed uint16
		for i := 0; i < 3; i++ {
			packed |= uint16(lang[i]-0x60) << (5 * uint(2-i))
		}
		binary.BigEndian.PutUint16(p[off:off+2], packed)
	}
	return mp4box("mdhd", p)
}

// stsd wraps sample entries behind a version/flags word and an entry count.
func stsd(entries ...[]byte) []byte {
	head := make([]byte, 8) // version+flags, entry count
	binary.BigEndian.PutUint32(head[4:8], uint32(len(entries)))
	return mp4box("stsd", append(head, concat(entries...)...))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// An audio sample entry: 16 bytes of reserved/version fields, then the channel count.
func audioEntry(fourCC string, channels int) []byte {
	p := make([]byte, 18)
	binary.BigEndian.PutUint16(p[16:18], uint16(channels))
	return mp4box(fourCC, p)
}

// A video sample entry. `extra` boxes live past the fixed 78-byte visual header — where dvcC/dvvC go.
func videoEntry(fourCC string, extra ...[]byte) []byte {
	return mp4box(fourCC, append(make([]byte, 78), concat(extra...)...))
}

func trak(kind, lang string, version byte, entries ...[]byte) []byte {
	mdia := mp4box("mdia",
		hdlr(kind),
		mdhd(lang, version),
		mp4box("minf", mp4box("stbl", stsd(entries...))),
	)
	return mp4box("trak", mdia)
}

func mp4File(traks ...[]byte) []byte {
	return append(mp4box("ftyp", make([]byte, 8)), mp4box("moov", concat(traks...))...)
}

func TestParseMP4_readsTracksLanguagesAndCodec(t *testing.T) {
	head := mp4File(
		trak("vide", "und", 0, videoEntry("hvc1")),
		trak("soun", "eng", 0, audioEntry("mp4a", 6)),
		trak("soun", "swe", 0, audioEntry("mp4a", 2)),
		trak("subt", "fin", 0),
	)
	p, ok := parseMP4(head)
	if !ok {
		t.Fatal("a well-formed file must parse")
	}
	if p.Container != "mp4" {
		t.Errorf("container = %q", p.Container)
	}
	if p.VideoCodec != "hevc" {
		t.Errorf("videoCodec = %q, want hevc", p.VideoCodec)
	}
	// mdhd stores ISO 639-2 ("eng"); the probe normalises to 639-1 so one vocabulary reaches Den,
	// whatever the container spoke.
	if len(p.Audio) != 2 || p.Audio[0] != "en" || p.Audio[1] != "sv" {
		t.Errorf("audio = %v, want [en sv]", p.Audio)
	}
	if len(p.Subtitles) != 1 || p.Subtitles[0] != "fi" {
		t.Errorf("subtitles = %v, want [fi]", p.Subtitles)
	}
	// The channel count comes from the FIRST audio track's sample entry.
	if p.AudioChannels != "5.1" {
		t.Errorf("audioChannels = %q, want 5.1", p.AudioChannels)
	}
	// A video track tagged `und` is not a language claim, and must not appear as one.
	if p.UntaggedAudio != 0 {
		t.Errorf("untaggedAudio = %d, want 0", p.UntaggedAudio)
	}
}

// An untagged audio track is COUNTED, never guessed at. Den treats untagged as "probably the original
// language" downstream; inventing "eng" here would announce English audio for a Swedish show.
func TestParseMP4_untaggedTracksAreCountedNotNamed(t *testing.T) {
	head := mp4File(
		trak("soun", "", 0, audioEntry("mp4a", 2)),
		trak("soun", "und", 0, audioEntry("mp4a", 2)),
		trak("sbtl", "", 0),
	)
	p, ok := parseMP4(head)
	if !ok {
		t.Fatal("parse")
	}
	if len(p.Audio) != 0 {
		t.Errorf("audio = %v, want none named", p.Audio)
	}
	if p.UntaggedAudio != 2 {
		t.Errorf("untaggedAudio = %d, want 2 (empty and 'und' both count)", p.UntaggedAudio)
	}
	if p.UntaggedSubtitles != 1 {
		t.Errorf("untaggedSubtitles = %d, want 1", p.UntaggedSubtitles)
	}
}

// mdhd v1 widens created/modified/duration to 64-bit, moving the language field. Reading it at the v0
// offset yields three garbage bytes, which is how a file gets a language nobody wrote.
func TestParseMP4_mdhdVersion1LanguageOffset(t *testing.T) {
	p, ok := parseMP4(mp4File(trak("soun", "ger", 1, audioEntry("mp4a", 2))))
	if !ok {
		t.Fatal("parse")
	}
	if len(p.Audio) != 1 || p.Audio[0] != "de" {
		t.Errorf("audio = %v, want [de] from a v1 mdhd", p.Audio)
	}
}

func TestParseMP4_dolbyVisionConfigBox(t *testing.T) {
	plain, _ := parseMP4(mp4File(trak("vide", "und", 0, videoEntry("hvc1"))))
	if plain.DolbyVision {
		t.Error("no dvcC/dvvC → not Dolby Vision")
	}
	for _, box := range []string{"dvcC", "dvvC"} {
		got, ok := parseMP4(mp4File(trak("vide", "und", 0, videoEntry("hvc1", mp4box(box, make([]byte, 4))))))
		if !ok || !got.DolbyVision {
			t.Errorf("%s present → Dolby Vision (ok=%v, dv=%v)", box, ok, got.DolbyVision)
		}
	}
}

// Every way a head can fail to be a parseable MP4. None may panic, and none may report a guess.
func TestParseMP4_rejectsWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		head []byte
	}{
		{"empty", nil},
		{"shorter than a box header", []byte{0, 0, 0, 1}},
		{"not ftyp", append(mp4box("junk", make([]byte, 8)), mp4box("moov")...)},
		{"ftyp but no moov", mp4box("ftyp", make([]byte, 8))},
		{"moov with no tracks", mp4File()},
		{"a track with no mdia", append(mp4box("ftyp", make([]byte, 8)), mp4box("moov", mp4box("trak"))...)},
		{"a track that describes nothing", mp4File(trak("vide", "", 0))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p, ok := parseMP4(c.head); ok {
				t.Errorf("reported %+v for %s — silence beats a guess", p, c.name)
			}
		})
	}
}

// The box walker is the part that reads sizes out of untrusted bytes. A wrong size must stop the walk,
// never index outside the buffer.
func TestBoxes_sizeHandling(t *testing.T) {
	t.Run("64-bit size", func(t *testing.T) {
		payload := make([]byte, 4)
		body := make([]byte, 8)
		binary.BigEndian.PutUint64(body, uint64(16+len(payload))) // header(8) + widesize(8) + payload
		buf := make([]byte, 8)
		binary.BigEndian.PutUint32(buf[0:4], 1) // size==1 → the real size is 64-bit and follows the type
		copy(buf[4:8], "wide")
		buf = append(buf, append(body, payload...)...)

		got := boxes(buf)
		if len(got) != 1 || got[0].typ != "wide" || len(got[0].body) != len(payload) {
			t.Fatalf("64-bit sized box mis-read: %+v", got)
		}
	})

	t.Run("size 0 runs to the end", func(t *testing.T) {
		buf := make([]byte, 8)
		copy(buf[4:8], "last")
		buf = append(buf, 1, 2, 3, 4)
		got := boxes(buf)
		if len(got) != 1 || len(got[0].body) != 4 {
			t.Fatalf("size-0 box must extend to the buffer end: %+v", got)
		}
	})

	t.Run("truncated and impossible sizes stop the walk", func(t *testing.T) {
		bad := [][]byte{
			{0, 0, 0, 0xFF, 'b', 'o', 'x', 'x', 1, 2}, // size past the end
			{0, 0, 0, 4, 'b', 'o', 'x', 'x'},          // size smaller than its own header
			{0, 0, 0, 1, 'b', 'o', 'x', 'x'},          // 64-bit flagged, no room for the width
		}
		for i, buf := range bad {
			if got := boxes(buf); len(got) != 0 {
				t.Errorf("case %d: walked into a bad size: %+v", i, got)
			}
		}
	})
}

func TestFourCCAndChannelMapping(t *testing.T) {
	codecs := map[string]string{
		"XVID": "mpeg4", "divx": "mpeg4", "DX50": "mpeg4", "mp4v": "mpeg4", "FMP4": "mpeg4",
		"avc1": "h264", "H264": "h264", "x264": "h264",
		"hvc1": "hevc", "HEV1": "hevc", "x265": "hevc",
		"av01": "av1", "vp09": "vp9", "VP90": "vp9",
		// Anything unrecognised must stay empty rather than becoming a wrong tag — Den's autoPickRank
		// demotes on the codec name, so a bad guess mis-ranks a release it can't decode.
		"zzzz": "", "": "",
	}
	for cc, want := range codecs {
		if got := codecFromFourCC(cc); got != want {
			t.Errorf("codecFromFourCC(%q) = %q, want %q", cc, got, want)
		}
	}

	layouts := map[int]string{0: "", 1: "1.0", 2: "2.0", 5: "2.0", 6: "5.1", 7: "5.1", 8: "7.1", 12: "7.1"}
	for n, want := range layouts {
		if got := channelLayout(n); got != want {
			t.Errorf("channelLayout(%d) = %q, want %q", n, got, want)
		}
	}
	// channelCount is the inverse, used to rank one layout against another; unknown text is 0, not a guess.
	for _, layout := range []string{"7.1", "5.1", "2.0", "1.0"} {
		if channelCount(layout) != channelCountRoundTrip(layout) {
			t.Errorf("channelCount(%q) disagrees with its layout", layout)
		}
	}
	if channelCount("atmos") != 0 || channelCount("") != 0 {
		t.Error("an unrecognised layout counts as 0 channels, never a guess")
	}
}

// The smallest channel count that produces this layout — channelLayout(channelCount(x)) must be x.
func channelCountRoundTrip(layout string) int {
	switch layout {
	case "7.1":
		return 8
	case "5.1":
		return 6
	case "2.0":
		return 2
	case "1.0":
		return 1
	}
	return 0
}

// Real files, from a real muxer. The synthetic boxes above pin the EDGE cases a muxer will never
// produce (a truncated size, a 64-bit header, a v1 mdhd); these pin that the parser agrees with what
// ffmpeg actually writes — the thing a hand-built fixture can quietly drift away from.
//
// Regenerate with (see testdata/README.md):
//
//	ffmpeg -f lavfi -i testsrc=size=64x64:rate=5:duration=1 \
//	       -f lavfi -i sine=frequency=440:duration=1 -f lavfi -i sine=frequency=880:duration=1 \
//	       -map 0:v -map 1:a -map 2:a -c:v libx264 -preset ultrafast -c:a aac -ac:a:0 6 -ac:a:1 2 \
//	       -metadata:s:a:0 language=swe -metadata:s:a:1 language=ita -movflags +faststart multi-audio.mp4
func TestParseMP4_realFileFromFFmpeg(t *testing.T) {
	p, ok := parseMP4(readFixture(t, "multi-audio.mp4"))
	if !ok {
		t.Fatal("a real ffmpeg mp4 must parse")
	}
	if p.VideoCodec != "h264" {
		t.Errorf("videoCodec = %q, want h264 (avc1)", p.VideoCodec)
	}
	if len(p.Audio) != 2 || p.Audio[0] != "sv" || p.Audio[1] != "it" {
		t.Errorf("audio = %v, want [sv it] — the languages ffmpeg was told to tag", p.Audio)
	}
	// The first audio track was muxed as 5.1; the layout must come from the file, not the release name.
	if p.AudioChannels != "5.1" {
		t.Errorf("audioChannels = %q, want 5.1", p.AudioChannels)
	}
	if p.UntaggedAudio != 0 {
		t.Errorf("untaggedAudio = %d — both tracks carry a language", p.UntaggedAudio)
	}
	if p.DolbyVision {
		t.Error("a plain h264 file is not Dolby Vision")
	}
}

// The case the parser documents and declines: a muxer that leaves `moov` at the END. Production reads a
// bounded head, so the metadata is simply not there — and reporting nothing is right, because the
// alternative is describing a file from whatever bytes happened to be in reach.
func TestParseMP4_moovBeyondTheHeadReportsNothing(t *testing.T) {
	full := readFixture(t, "moov-at-end.mp4")
	if p, ok := parseMP4(full); !ok || len(p.Audio) != 1 || p.Audio[0] != "fr" {
		t.Fatalf("the whole file still parses: ok=%v probe=%+v", ok, p)
	}
	// The first 4 KiB is all a probe of a slow source may get before its budget runs out.
	if p, ok := parseMP4(full[:4096]); ok {
		t.Errorf("described a file whose moov it never saw: %+v", p)
	}
}

// readFixture loads a testdata file, failing the test rather than the parser when it is missing.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return data
}
