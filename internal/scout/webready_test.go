package scout

import "testing"

// notWebReady follows Stremio's definition: only an https URL to an MP4 is web-ready, and the probed
// container wins over the release name.
func TestWebReady(t *testing.T) {
	mkv := &Probe{Container: "matroska"}
	mp4 := &Probe{Container: "mp4"}
	for _, c := range []struct {
		name  string
		s     RawStream
		url   string
		ready bool
	}{
		{"https mp4 by name", RawStream{Title: "Movie.2020.1080p.WEB-DL.AAC.mp4"}, "https://s/p/t", true},
		{"https m4v by name", RawStream{Title: "Movie.m4v"}, "https://s/p/t", true},
		{"https mkv by name", RawStream{Title: "Movie.2020.2160p.REMUX.TrueHD.mkv"}, "https://s/p/t", false},
		{"no extension", RawStream{Title: "Movie 2020 1080p WEB-DL"}, "https://s/p/t", false},
		{"http mp4", RawStream{Title: "Movie.mp4"}, "http://192.168.86.193:8080/p/t", false},
		{"probe says mkv despite .mp4 name", RawStream{Title: "Movie.mp4", Probe: mkv}, "https://s/p/t", false},
		{"probe says mp4 without an extension", RawStream{Title: "Movie 1080p", Probe: mp4}, "https://s/p/t", true},
		{"probe with no container falls back to the name", RawStream{Title: "Movie.mp4", Probe: &Probe{}}, "https://s/p/t", true},
	} {
		if got := webReady(c.s, c.url); got != c.ready {
			t.Errorf("%s: webReady = %v, want %v", c.name, got, c.ready)
		}
	}
}
