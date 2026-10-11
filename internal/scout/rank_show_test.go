package scout

import "testing"

// Pokémon S1E1 as the indexers answered it on 2026-10-11, and the Horizons release Den Web auto-played.
// Every name here is from that list; the only thing invented is which ones are cached.
func TestRankStreams_aSpinOffOrAnotherEpisodeSinksBelowTheShow(t *testing.T) {
	cached := func(s *RawStream) { s.Cached, s.CacheKnown = true, true }
	streams := []RawStream{
		rs("Pokemon.Horizons.The.Series.S01E01.The.Pendant.That.Starts.It.All.Part.One.1080p.NF.WEB-DL.AAC2.0.H.264-VARYG.mkv", cached),
		rs("Pokemon.Master.Journeys.The.Series.S01E01.to.Train.or.Not.to.Train!.1080p.NF.WEB-DL.DDP2.0.x264-NanDesuKa.mkv", cached),
		rs("www.UIndex.org - Pokemon S00E82 Distant Blue Sky 1080p AMZN WEB-DL DDP2 0 H 264-Kitsune", cached),
		rs("Pokémon Horizons - 01 (1920x1080 - Cartoon Network CA).mkv", cached),
		rs("Pokémon (1997) - S01E01 - Pokémon - I Choose You! (1080p BluRay x265 ImE).mkv", nil),
		rs("Pokemon - Temporada 1 - 001.mp4", nil),
	}
	ranked := rankStreams(streams, rankFilters{
		ResultCap:  10,
		ShowTokens: titleTokens("Pokémon"),
		Episode:    &[2]int{1, 1},
	})
	got := titles(ranked)
	if got[0] != streams[4].Title {
		t.Fatalf("the 1997 show's own S01E01 must lead, uncached or not; got %v", got)
	}
	if got[1] != streams[5].Title {
		t.Errorf("a season word and number between show and episode name neither; got %v", got)
	}
}

func TestOtherShow(t *testing.T) {
	expected := titleTokens("Pokémon")
	for title, want := range map[string]bool{
		"Pokemon.Horizons.The.Series.S01E01.1080p.mkv":         true,
		"Pokémon Horizons - 01 (1920x1080).mkv":                true,
		"[DragsterPS] Pokémon Indigo League E01 [1080p].mkv":   true, // a season name reads as a spin-off: sunk, not dropped
		"Pokémon (1997) - S01E01 - Pokémon - I Choose You.mkv": false,
		"Pokemon S01E01 Pokemon, I Choose You!.mkv":            false,
		"EP01 Pokémon! Minä valitsen sinut!.mp4":               false, // the marker comes first: nothing to judge
		"[PM]Pocket_Monsters_Best_Wishes_001.mkv":              false, // no marker this reads
	} {
		if got := otherShow(title, expected); got != want {
			t.Errorf("otherShow(%q) = %v, want %v", title, got, want)
		}
	}
}

func TestOtherEpisode(t *testing.T) {
	for title, want := range map[string]bool{
		"Pokemon S00E82 Distant Blue Sky 1080p": true,
		"Pokemon.S01E01.1080p.mkv":              false,
		"Pokemon.S01E01-E10.1080p":              false, // a range holding it names it
		"Pokemon.S01.Complete.1080p":            false, // a pack labels no episode
		"Pokemon - 001.mkv":                     false,
	} {
		if got := otherEpisode(title, 1, 1); got != want {
			t.Errorf("otherEpisode(%q) = %v, want %v", title, got, want)
		}
	}
}

func TestTitleTokens_foldsDiacritics(t *testing.T) {
	if !titleOverlap("Pokemon.S04E25.From.Ghost.to.Ghost.mkv", titleTokens("Pokémon")) {
		t.Error("Pokémon and Pokemon are the same word")
	}
}
