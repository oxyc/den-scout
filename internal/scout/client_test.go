package scout

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A browser that decodes 4K HDR HEVC and AV1 but no Dolby Vision or E-AC-3, as Chrome on a Mac reports.
func aBrowser(change func(*ClientPlayable)) *ClientPlayable {
	p := &ClientPlayable{H264: 51, HEVCMain: 153, HEVCMain10: 153, HDR: true, AACMultichannel: true,
		AV1: 13, AV1Main10: 13, AV1HDR: true}
	if change != nil {
		change(p)
	}
	return p
}

func TestRankStreams_forTheBrowserThatAsked(t *testing.T) {
	remux := rs("Film.2024.2160p.UHD.BluRay.REMUX.HEVC.HDR.TrueHD.7.1.mkv", func(s *RawStream) { s.InfoHash = "remux" })
	dv5 := rs("Film.2024.2160p.WEB-DL.DV.HEVC.AAC.mkv", func(s *RawStream) {
		s.InfoHash = "dv5"
		s.Probe = &Probe{Container: "matroska", VideoCodec: "hevc", DolbyVision: true, DVProfile: 5, BitDepth: 10}
	})
	web := rs("Film.2024.1080p.WEB-DL.H.264.AAC.mkv", func(s *RawStream) { s.InfoHash = "web" })
	xvid := rs("Film.2024.DVDRip.XviD.MP3.avi", func(s *RawStream) { s.InfoHash = "xvid" })
	all := []RawStream{xvid, web, dv5, remux}
	rank := func(client *ClientPlayable) []string {
		return titles(rankStreams(all, rankFilters{ResultCap: 10, Client: client}))
	}

	tv := rank(nil)
	if tv[0] != remux.Title {
		t.Errorf("the TV's list starts %q, want the 4K remux", tv[0])
	}

	for _, c := range []struct {
		name   string
		client *ClientPlayable
		want   []string
	}{
		{"4K HDR HEVC: the remux only needs its audio converted", aBrowser(nil),
			[]string{remux.Title, web.Title, dv5.Title, xvid.Title}},
		{"no HEVC: the remux has to be transcoded, so the 1080p H.264 plays first",
			aBrowser(func(p *ClientPlayable) { p.HEVCMain, p.HEVCMain10 = 0, 0 }),
			[]string{web.Title, remux.Title, dv5.Title, xvid.Title}},
		{"no HDR: the same", aBrowser(func(p *ClientPlayable) { p.HDR = false }),
			[]string{web.Title, remux.Title, dv5.Title, xvid.Title}},
		{"Dolby Vision 5 shown: the 4K web release copies whole, ahead of the remux whose TrueHD is converted",
			aBrowser(func(p *ClientPlayable) { p.DolbyVision.P5 = true }),
			[]string{dv5.Title, remux.Title, web.Title, xvid.Title}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rank(c.client); strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("order\n got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestClientPlayable_cost(t *testing.T) {
	p := aBrowser(nil)
	for _, c := range []struct {
		title string
		want  int
	}{
		{"Film.1080p.WEB-DL.H.264.AAC.mkv", 0},
		{"Film.1080p.WEB-DL.H.264.DDP5.1.mkv", audioConvertedCost},
		{"Film.1080p.WEB-DL.AAC.mkv", videoUnnamedCost},
		{"Film.2160p.WEB-DL.H.264.AAC.mkv", 0},
		{"Film.1080p.Hi10P.AAC.mkv", neverPlaysCost},
		{"Film.2160p.WEB-DL.AV1.AAC.mkv", 0},
		{"Film.1080p.WEB-DL.AV1.AAC.webm", neverPlaysCost},
		{"Film.1080p.BluRay.H.264.AAC.3D.HSBS.mkv", neverPlaysCost},
		{"Film.1080p.BluRay.H.264.AAC.m2ts", neverPlaysCost},
	} {
		if got := p.cost(RawStream{Title: c.title}, streamAttributes(RawStream{Title: c.title})); got != c.want {
			t.Errorf("cost(%q) = %d, want %d", c.title, got, c.want)
		}
	}
	eac3 := aBrowser(func(p *ClientPlayable) { p.EAC3 = true })
	if got := eac3.cost(RawStream{Title: "Film.1080p.WEB-DL.H.264.DDP5.1.mkv"},
		streamAttributes(RawStream{Title: "Film.1080p.WEB-DL.H.264.DDP5.1.mkv"})); got != 0 {
		t.Errorf("E-AC-3 for a player that plays it costs %d, want 0: it is copied", got)
	}
}

func TestStreamList_rankedForTheBrowserThatAsked(t *testing.T) {
	seeds := func(context.Context) ([]RawStream, error) {
		return []RawStream{
			{InfoHash: repeat("a", 40), Title: "Movie 2160p WEB-DL HDR HEVC AAC", SizeBytes: intp(18 * gib), Seeders: intp(100), FileIdx: intp(0)},
			{InfoHash: repeat("b", 40), Title: "Movie 1080p WEB-DL H.264 AAC", SizeBytes: intp(8 * gib), Seeders: intp(50), FileIdx: intp(0)},
		}, nil
	}
	h := NewHandler(testDeps(func(d *Deps) {
		d.MakeScrapers = func(*Config) []scraper { return []scraper{fakeScraper{"torrentio", seeds}} }
	}))
	path := "/" + validBlob + "/stream/movie/tt1234567.json"
	first := func(rr *httptest.ResponseRecorder) string {
		t.Helper()
		var list streamsResponse
		if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &list) != nil || len(list.Streams) == 0 {
			t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
		}
		return list.Streams[0].Title
	}
	noHEVC, _ := json.Marshal(aBrowser(func(p *ClientPlayable) { p.HEVCMain, p.HEVCMain10 = 0, 0 }))

	tv := do(h, path, nil)
	if got := first(tv); !strings.Contains(got, "2160p") {
		t.Errorf("the TV's list starts %q, want the 4K release", got)
	}
	if !strings.Contains(tv.Header().Get("vary"), playableHeader) {
		t.Errorf("vary = %q, want it to name %s", tv.Header().Get("vary"), playableHeader)
	}
	if got := first(do(h, path, map[string]string{playableHeader: string(noHEVC)})); !strings.Contains(got, "1080p") {
		t.Errorf("a browser without HEVC gets %q first, want the 1080p H.264", got)
	}
	if got := first(do(h, path, nil)); !strings.Contains(got, "2160p") {
		t.Errorf("after a browser's list the TV gets %q first: the two lists must be cached apart", got)
	}
	if got := first(do(h, path, map[string]string{playableHeader: "{not json"})); !strings.Contains(got, "2160p") {
		t.Errorf("a report that doesn't parse gets %q first, want the TV's list", got)
	}
}
