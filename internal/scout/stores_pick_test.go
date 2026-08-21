package scout

import "testing"

// Picking a file out of a season pack is the step where a mistake is invisible until playback: the
// stream plays, it is simply the wrong episode. Each store picks differently when the name match fails
// (TorBox trusts the given index, RD falls back to the first file, Premiumize to the largest), so each
// needs its own table rather than one shared assumption.

func file(idx int, name string, size int) TorrentFile {
	return TorrentFile{Index: idx, Name: name, SizeBytes: &size}
}

func ep(season, episode int) ResolveTarget {
	return ResolveTarget{InfoHash: H, Season: &season, Episode: &episode}
}

func idx(i int) *int { return &i }

func wantPick(t *testing.T, got *int, want int, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil, want %d", what, want)
	}
	if *got != want {
		t.Errorf("%s: got %d, want %d", what, *got, want)
	}
}

// The pack is the common case: name-matching must beat the positional index, because the indexer's
// fileIdx and the debrid's own file order are not guaranteed to agree.
func TestPickEpisodeFile_nameMatchBeatsOrder(t *testing.T) {
	files := []TorrentFile{
		file(0, "Show.S01E01.1080p.mkv", 900),
		file(1, "Show.S01E02.1080p.mkv", 950),
		file(2, "Show.S01E03.1080p.mkv", 1000),
	}
	wantPick(t, pickEpisodeFile(files, 1, 2), 1, "S01E02")
	wantPick(t, pickEpisodeFile(files, 1, 3), 2, "S01E03")
	// No match is NOT nil: it falls back to the largest video. That fallback is what makes a
	// single-episode torrent work at all, since its filename names no episode to match against.
	wantPick(t, pickEpisodeFile(files, 2, 1), 2, "no match falls back to the largest video")
}

// Samples and extras carry the episode number too. The real episode is the big file.
func TestPickEpisodeFile_prefersTheLargestMatch(t *testing.T) {
	files := []TorrentFile{
		file(0, "Show.S01E02.sample.mkv", 20),
		file(1, "Show.S01E02.1080p.mkv", 4000),
	}
	wantPick(t, pickEpisodeFile(files, 1, 2), 1, "the sample must lose to the episode")
}

// Non-video files (nfo, subs, artwork) are excluded — but a pack of ONLY non-video must still be
// searchable rather than answering nil, or a legitimate odd container becomes unplayable.
func TestPickEpisodeFile_videosFirstThenAnything(t *testing.T) {
	mixed := []TorrentFile{
		file(0, "Show.S01E02.nfo", 1),
		file(1, "Show.S01E02.mkv", 500),
	}
	wantPick(t, pickEpisodeFile(mixed, 1, 2), 1, "the video wins over the nfo")

	onlyOther := []TorrentFile{file(0, "Show.S01E02.iso", 500)}
	wantPick(t, pickEpisodeFile(onlyOther, 1, 2), 0, "with no video extension, fall back to the pool")

	if got := pickEpisodeFile(nil, 1, 2); got != nil {
		t.Errorf("an empty pack has nothing to pick: got %d", *got)
	}
}

// TorBox: name match first, then the caller's index, and an out-of-range index is handed back untouched
// (it is the debrid's own id space, not a slice offset).
func TestSelectFileID_torBox(t *testing.T) {
	files := []TorrentFile{file(0, "a.mkv", 10), file(1, "b.mkv", 20)}

	// Neither file names an episode, so the match falls back to the largest video rather than failing.
	wantPick(t, selectFileID(files, ep(1, 1)), 1, "no name match → largest video")
	if got := selectFileID(files, ResolveTarget{InfoHash: H}); got != nil {
		t.Errorf("no episode and no index → nothing to select: got %d", *got)
	}

	inRange := ResolveTarget{InfoHash: H, FileIdx: idx(1)}
	wantPick(t, selectFileID(files, inRange), 1, "an in-range index maps through the pack")

	out := ResolveTarget{InfoHash: H, FileIdx: idx(9)}
	wantPick(t, selectFileID(files, out), 9, "an out-of-range index is passed through as an id")

	named := []TorrentFile{file(7, "Show.S02E05.mkv", 100), file(8, "Show.S02E06.mkv", 100)}
	wantPick(t, selectFileID(named, ep(2, 6)), 8, "the name match wins over any index")
}

// Real-Debrid: same name-match precedence, but an unusable index falls back to the FIRST file rather
// than to nothing — RD always needs a concrete file id to unrestrict.
func TestPickFileID_realDebrid(t *testing.T) {
	s := &realDebridStore{}
	files := []TorrentFile{file(3, "a.mkv", 10), file(4, "b.mkv", 20)}

	if got := s.pickFileID(nil, ep(1, 1)); got != nil {
		t.Errorf("no files → no pick: got %d", *got)
	}
	wantPick(t, s.pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(1)}), 4, "in-range index")
	wantPick(t, s.pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(99)}), 3, "out of range → first file")

	named := []TorrentFile{file(3, "Show.S01E04.mkv", 10), file(4, "Show.S01E05.mkv", 20)}
	wantPick(t, s.pickFileID(named, ep(1, 5)), 4, "the name match wins")
}

// Premiumize: an unusable index falls back to the LARGEST file — with no id space of its own, the
// biggest file in a pack is the best guess at the feature.
func TestPickIndex_premiumize(t *testing.T) {
	s := &premiumizeStore{}
	files := []TorrentFile{file(0, "small.mkv", 10), file(1, "big.mkv", 9000)}

	if got := s.pickIndex(nil, ep(1, 1)); got != nil {
		t.Errorf("no files → no pick: got %d", *got)
	}
	wantPick(t, s.pickIndex(files, ResolveTarget{InfoHash: H, FileIdx: idx(0)}), 0, "in-range index")
	wantPick(t, s.pickIndex(files, ResolveTarget{InfoHash: H, FileIdx: idx(42)}), 1, "out of range → largest")
	wantPick(t, s.pickIndex(files, ResolveTarget{InfoHash: H}), 1, "no index at all → largest")

	named := []TorrentFile{file(0, "Show.S03E01.mkv", 10), file(1, "Show.S03E02.mkv", 20)}
	wantPick(t, s.pickIndex(named, ep(3, 1)), 0, "the name match wins over the largest")
}

// A negative index is untrusted input from an addon, not an offset — it must never index backwards.
func TestPickers_rejectNegativeIndex(t *testing.T) {
	files := []TorrentFile{file(0, "a.mkv", 10), file(1, "b.mkv", 20)}
	neg := ResolveTarget{InfoHash: H, FileIdx: idx(-1)}

	wantPick(t, selectFileID(files, neg), -1, "TorBox hands a negative back as an id, never as an offset")
	wantPick(t, (&realDebridStore{}).pickFileID(files, neg), 0, "RD falls back to the first file")
	wantPick(t, (&premiumizeStore{}).pickIndex(files, neg), 1, "Premiumize falls back to the largest")
}

// The two stores with no status API must say so — and be honest that they are saying nothing, so the
// pool moves on to one that can answer instead of reporting a wait nobody is serving.
func TestStoresWithoutStatusReportNothing(t *testing.T) {
	for _, st := range []Store{&realDebridStore{}, &premiumizeStore{}} {
		if _, ok := st.Status(t.Context(), ResolveTarget{InfoHash: H}); ok {
			t.Errorf("%s claims a download status it cannot know", st.Service())
		}
	}
}

// Each store names itself — the pool's ordering and the cache-truth rule both key on this.
func TestStoresIdentifyThemselves(t *testing.T) {
	cases := []struct {
		store Store
		want  DebridService
	}{
		{&torBoxStore{}, ServiceTorBox},
		{&realDebridStore{}, ServiceRealDebrid},
		{&premiumizeStore{}, ServicePremiumize},
	}
	for _, c := range cases {
		if got := c.store.Service(); got != c.want {
			t.Errorf("Service() = %q, want %q", got, c.want)
		}
	}
	// Only TorBox and Premiumize have a real cache API; RD contributing "cache truth" would make an
	// all-false answer look authoritative and empty every cached-only list.
	if !isCacheTruthService(ServiceTorBox) || !isCacheTruthService(ServicePremiumize) {
		t.Error("TorBox and Premiumize are the cache-truth services")
	}
	if isCacheTruthService(ServiceRealDebrid) {
		t.Error("RD has no cache API — it must never count as cache truth")
	}
}

// The error a dead link raises is matched by string in places; keep its shape asserted.
func TestDeadLinkErrorMessage(t *testing.T) {
	err := &DeadLinkError{"torbox createtorrent http 429"}
	if got := err.Error(); got != "dead_link: torbox createtorrent http 429" {
		t.Errorf("Error() = %q", got)
	}
}
