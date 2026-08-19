package scout

import "testing"
import "os"

// AVI records no language anywhere, but it does name its codec — and that is the fact that matters most
// here: an XviD rip decodes in software on an Apple TV, which costs the viewer the scrubber and the whole
// native transport. Fixture is a real head from a release the addon offered.
func TestParseHead_avi(t *testing.T) {
	head, err := os.ReadFile("testdata/xvid.avi")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseHead(head)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Container != "avi" || got.VideoCodec != "mpeg4" {
		t.Fatalf("container=%q codec=%q, want avi/mpeg4", got.Container, got.VideoCodec)
	}
	if len(got.Audio) != 0 {
		t.Fatalf("audio = %v, want none — AVI carries no language metadata", got.Audio)
	}
	if got.UntaggedAudio == 0 {
		t.Fatal("an audio track exists and should be counted even though it has no language")
	}
}

// A real MP4, so the box walking and mdhd's packed language are exercised against something a muxer
// actually produced.
func TestParseHead_mp4(t *testing.T) {
	head, err := os.ReadFile("testdata/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseHead(head)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Container != "mp4" {
		t.Fatalf("container = %q, want mp4", got.Container)
	}
	if got.VideoCodec != "h264" {
		t.Fatalf("codec = %q, want h264", got.VideoCodec)
	}
}

// Matroska still routes correctly once the other two parsers are ahead of it.
func TestParseHead_matroskaStillWins(t *testing.T) {
	head, err := os.ReadFile("testdata/tracks.mkv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseHead(head)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Container != "matroska" || len(got.Audio) != 1 || got.Audio[0] != "it" {
		t.Fatalf("got %+v, want matroska with [it]", got)
	}
}

// An unrecognised head is an honest failure, not an empty success.
func TestParseHead_unknown(t *testing.T) {
	if _, err := ParseHead([]byte("this is not a media container")); err == nil {
		t.Fatal("want an error for an unrecognised container")
	}
}
