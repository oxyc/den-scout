package scout

import (
	"errors"
	"os"
	"testing"
)

// The parser is what makes the picker honest, so it is tested against a REAL head — 5 KB cut from a
// release TorBox actually served, not a synthetic file. That release is the reason this exists: offered
// for a Swedish series, it carries Italian audio and no subtitles, and nothing in its title said so.
func TestParseMatroskaTracks_realHead(t *testing.T) {
	head, err := os.ReadFile("testdata/tracks.mkv")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got, err := ParseMatroskaTracks(head)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Audio) != 1 || got.Audio[0] != "ita" {
		t.Fatalf("audio = %v, want [ita]", got.Audio)
	}
	if len(got.Subtitles) != 0 {
		t.Fatalf("subtitles = %v, want none", got.Subtitles)
	}
}

// A head with no Tracks element (an MP4 with its moov at the end, a truncated read) must say so rather
// than report an empty-but-successful result — the caller keeps the title's own badges instead.
func TestParseMatroskaTracks_noTracks(t *testing.T) {
	for name, buf := range map[string][]byte{
		"empty":     {},
		"garbage":   []byte("not a matroska file at all, just bytes"),
		"truncated": {0x1A, 0x45, 0xDF, 0xA3, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseMatroskaTracks(buf); !errors.Is(err, ErrNoTracks) {
				t.Fatalf("err = %v, want ErrNoTracks", err)
			}
		})
	}
}

// Matroska omits Language when the track is English, so an entry without the field is English by
// specification — not unknown, and not something to hide from the viewer.
func TestParseTrackEntry_defaultsToEnglish(t *testing.T) {
	entry := []byte{idTrackType, 0x81, trackTypeAudio}
	kind, lang := parseTrackEntry(entry)
	if kind != trackTypeAudio || lang != "eng" {
		t.Fatalf("kind=%d lang=%q, want audio/eng", kind, lang)
	}
}

// A regional tag and its base language are the same shelf to someone picking a track.
func TestCleanLang(t *testing.T) {
	cases := map[string]string{
		"swe\x00": "swe", "PT-BR": "pt", "en_US": "en", "und": "", "  ": "", "ita": "ita",
	}
	for in, want := range cases {
		if got := cleanLang([]byte(in)); got != want {
			t.Fatalf("cleanLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only audio and subtitle tracks are reported; video and the cover-art stream the sample carries are not
// languages anyone chooses between.
func TestParseTrackEntry_ignoresVideo(t *testing.T) {
	entry := []byte{idTrackType, 0x81, 0x01}
	if _, lang := parseTrackEntry(entry); lang != "" {
		t.Fatalf("video track reported language %q", lang)
	}
}
