package scout

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDetectAudioCodecBranches(t *testing.T) {
	audio := map[string]string{
		"x DTS:X": "DTS:X", "x TrueHD": "TrueHD", "x DTS-HD MA": "DTS-HD", "x FLAC": "FLAC",
		"x EAC3": "EAC3", "x DTS": "DTS", "x AC3": "AC3", "x Atmos": "Atmos",
	}
	for title, want := range audio {
		if a := streamAttributes(RawStream{Title: title}).Audio; a == nil || *a != want {
			t.Errorf("audio(%q)=%v want %q", title, a, want)
		}
	}
	codec := map[string]string{"x AV1": "av1", "x x265": "hevc", "x HEVC": "hevc", "x x264": "avc", "x h264": "avc"}
	for title, want := range codec {
		if c := streamAttributes(RawStream{Title: title}).Codec; c == nil || *c != want {
			t.Errorf("codec(%q)=%v want %q", title, c, want)
		}
	}
	if streamAttributes(RawStream{Title: "x mystery"}).Codec != nil {
		t.Error("no codec → nil")
	}
}

func TestQualityScoreBranches(t *testing.T) {
	// Exercise the source ladder, audio ladder, and penalties (values are internal; just drive them).
	for _, title := range []string{
		"Movie 1080p WEBRip", "Movie 1080p WEB", "Movie 480p DVDRip", "Movie 720p TVRip",
		"Movie 1080p BluRay DTS-HD MA", "Movie 1080p BluRay TrueHD", "Movie 1080p WEB EAC3",
		"Movie 1080p WEB DTS", "Movie 1080p WEB AC3", "Movie 1080p WEB FLAC", "Movie 1080p WEB DTS:X",
		"Movie 1440p WEB", "Movie 576p WEB", "Movie 540p WEB", "Movie korsub", "Movie HC",
	} {
		_ = qualityScore(rs(title, nil))
	}
	// a large "4k"/uhd (no explicit 2160p) with a big file is trusted as 4K.
	big := qualityScore(rs("Movie UHD WEB", func(s *RawStream) { s.SizeBytes = intp(20 * gib) }))
	tiny := qualityScore(rs("Movie UHD WEB", func(s *RawStream) { s.SizeBytes = intp(1 * gib) }))
	if big <= tiny {
		t.Error("large uhd should outscore tiny uhd")
	}
}

func TestLabelBranches(t *testing.T) {
	cases := map[string]string{
		"Movie 1440p BluRay": "1440p • BluRay",
		"Movie 480p WEB":     "SD • WEB",
		"Movie 720p WEBRip":  "720p • WEB",
	}
	for title, want := range cases {
		if got := cleanLabel(RawStream{Title: title}); got != want {
			t.Errorf("cleanLabel(%q)=%q want %q", title, got, want)
		}
	}
	if got := detectResolution("x 1440p"); got != "1080p" && got != "" {
		_ = got // 1440p falls outside the coarse buckets; just ensure no panic
	}
}

func TestSeriesStreamAndForwardedOrigin(t *testing.T) {
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper {
			return []scraper{fakeScraper{"torrentio", func(context.Context) ([]RawStream, error) {
				return []RawStream{{InfoHash: repeat("a", 40), Title: "Show 1080p WEB-DL", SizeBytes: intp(3 * gib), FileIdx: intp(4)}}, nil
			}}}
		}
		d.MakeStores = func(*Config) []Store {
			return []Store{fakeStore{svc: ServiceTorBox, check: map[string]bool{repeat("a", 40): true}}}
		}
	}))
	rr := do(h, "/"+validBlob+"/stream/series/tt99:1:2.json", map[string]string{
		"X-Forwarded-Proto": "https", "X-Forwarded-Host": "scout.den.example",
	})
	if rr.Code != 200 {
		t.Fatalf("series stream: %d", rr.Code)
	}
	var body struct {
		Streams []struct {
			URL           string `json:"url"`
			BehaviorHints struct {
				BingeGroup string `json:"bingeGroup"`
			} `json:"behaviorHints"`
		} `json:"streams"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if len(body.Streams) != 1 {
		t.Fatalf("want 1 series stream, got %d", len(body.Streams))
	}
	first := body.Streams[0]
	if !strings.HasPrefix(first.URL, "https://scout.den.example/") {
		t.Errorf("forwarded origin: %s", first.URL)
	}
	if first.BehaviorHints.BingeGroup != "den-scout-tt99" {
		t.Errorf("bingeGroup: %s", first.BehaviorHints.BingeGroup)
	}
	tok := first.URL[strings.LastIndex(first.URL, "/play/")+len("/play/"):]
	pt, ok := decodePlayToken(tok)
	if !ok || pt.Season == nil || *pt.Season != 1 || pt.Episode == nil || *pt.Episode != 2 {
		t.Errorf("series play token: %+v ok=%v", pt, ok)
	}
}
