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

// nestedAVI builds an AVI head that nests `depth` LIST chunks and puts a single audio strh at the
// bottom. Each level costs 12 bytes, so a megabyte of head buys tens of thousands of them — the shape a
// file would take if it were built to make the parser recurse rather than to be played.
func nestedAVI(depth int) []byte {
	head := []byte("RIFF\xff\xff\xff\xffAVI ")
	for i := 0; i < depth; i++ {
		// A size larger than the buffer, which the walker clamps to the end — so every level nests
		// rather than running out.
		head = append(head, "LIST\xf0\xff\xff\xffjunk"...)
	}
	// strh: an audio stream header. Read only if the walk actually descended this far.
	head = append(head, "strh\x08\x00\x00\x00audsxvid"...)
	return head
}

// The walk stops at a bounded depth. Nesting is the only quantity in the three container parsers that
// the input controls with no ceiling, and these are bytes a remote server chose.
func TestParseAVI_boundsNestingDepth(t *testing.T) {
	// Just inside the bound: the strh is reached and counted, so the bound is not simply refusing files.
	shallow, ok := parseAVI(nestedAVI(maxAVIDepth - 1))
	if !ok || shallow.UntaggedAudio != 1 {
		t.Fatalf("shallow nesting: ok=%v untaggedAudio=%d, want a track found", ok, shallow.UntaggedAudio)
	}
	// Past it: the walk returns before reaching the strh, so nothing is read and nothing is claimed.
	if deep, ok := parseAVI(nestedAVI(maxAVIDepth + 1)); ok || deep.UntaggedAudio != 0 {
		t.Fatalf("deep nesting: ok=%v untaggedAudio=%d, want the walk to have stopped", ok, deep.UntaggedAudio)
	}
	// A depth bomb the size of a real probe read returns instead of recursing tens of thousands deep.
	if _, ok := parseAVI(nestedAVI(80_000)); ok {
		t.Fatal("a depth bomb produced a probe result")
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
