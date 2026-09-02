package scout

import (
	"errors"
	"testing"
)

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

func wantPick(t *testing.T, got *int, err error, want int, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: got error %v, want file %d", what, err, want)
	}
	if got == nil {
		t.Fatalf("%s: got nil, want %d", what, want)
	}
	if *got != want {
		t.Errorf("%s: got %d, want %d", what, *got, want)
	}
}

// wantNotInTorrent asserts a picker REFUSED, rather than substituting some other file.
func wantNotInTorrent(t *testing.T, got *int, err error, what string) {
	t.Helper()
	if !errors.Is(err, errEpisodeNotInTorrent) {
		if got != nil {
			t.Fatalf("%s: served file %d instead of refusing (err %v)", what, *got, err)
		}
		t.Fatalf("%s: got err %v, want errEpisodeNotInTorrent", what, err)
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
	got, err := pickEpisodeFile(files, 1, 2)
	wantPick(t, got, err, 1, "S01E02")
	got, err = pickEpisodeFile(files, 1, 3)
	wantPick(t, got, err, 2, "S01E03")

	// Every file here NAMES an episode, and none of them is S02E01 — so this pack demonstrably does not
	// hold what was asked for. Falling back to the largest video served S01E03 as though it were S02E01:
	// the stream plays, it is simply the wrong episode, and nothing upstream can tell.
	got, err = pickEpisodeFile(files, 2, 1)
	wantNotInTorrent(t, got, err, "an episode-labelled pack that lacks the episode")
}

// The fallback survives where it is actually needed: a release whose filename names no episode at all is
// a single episode, and its one video is the only thing it could be. Killing this to fix the case above
// would break every untagged single-episode release.
func TestPickEpisodeFile_unlabelledStillFallsBack(t *testing.T) {
	single := []TorrentFile{file(0, "Show.1080p.WEB-DL.x264.mkv", 4000)}
	got, err := pickEpisodeFile(single, 4, 7)
	wantPick(t, got, err, 0, "an untagged release falls back to its video")

	// A year and a resolution are digit runs that the bare 102/0102 matchers can brush against; neither
	// makes a name episode-LABELLED, so this must still fall back rather than refuse.
	yearly := []TorrentFile{file(0, "Movie.2019.1080p.BluRay.mkv", 8000)}
	got, err = pickEpisodeFile(yearly, 1, 1)
	wantPick(t, got, err, 0, "a year/resolution is not an episode label")
}

// Samples and extras carry the episode number too. The real episode is the big file.
func TestPickEpisodeFile_prefersTheLargestMatch(t *testing.T) {
	files := []TorrentFile{
		file(0, "Show.S01E02.sample.mkv", 20),
		file(1, "Show.S01E02.1080p.mkv", 4000),
	}
	got, err := pickEpisodeFile(files, 1, 2)
	wantPick(t, got, err, 1, "the sample must lose to the episode")
}

// Non-video files (nfo, subs, artwork) are excluded — but a pack of ONLY non-video must still be
// searchable rather than answering nil, or a legitimate odd container becomes unplayable.
func TestPickEpisodeFile_videosFirstThenAnything(t *testing.T) {
	mixed := []TorrentFile{
		file(0, "Show.S01E02.nfo", 1),
		file(1, "Show.S01E02.mkv", 500),
	}
	got, err := pickEpisodeFile(mixed, 1, 2)
	wantPick(t, got, err, 1, "the video wins over the nfo")

	onlyOther := []TorrentFile{file(0, "Show.S01E02.iso", 500)}
	got, err = pickEpisodeFile(onlyOther, 1, 2)
	wantPick(t, got, err, 0, "with no video extension, fall back to the pool")

	// Nothing to pick, and nothing to be wrong about — an empty list is not a mismatch.
	got, err = pickEpisodeFile(nil, 1, 2)
	if got != nil || err != nil {
		t.Errorf("an empty pack has nothing to pick: got %v, %v", got, err)
	}
}

// TorBox: name match first, then the caller's index, and an out-of-range index is handed back untouched
// (it is the debrid's own id space, not a slice offset).
func TestSelectFileID_torBox(t *testing.T) {
	files := []TorrentFile{file(0, "a.mkv", 10), file(1, "b.mkv", 20)}

	// Neither file names an episode, so the match falls back to the largest video rather than failing.
	got, err := selectFileID(files, ep(1, 1))
	wantPick(t, got, err, 1, "no name match → largest video")
	if got, err := selectFileID(files, ResolveTarget{InfoHash: H}); got != nil || err != nil {
		t.Errorf("no episode and no index → nothing to select: got %v, %v", got, err)
	}

	inRange := ResolveTarget{InfoHash: H, FileIdx: idx(1)}
	got, err = selectFileID(files, inRange)
	wantPick(t, got, err, 1, "an in-range index maps through the pack")

	out := ResolveTarget{InfoHash: H, FileIdx: idx(9)}
	got, err = selectFileID(files, out)
	wantPick(t, got, err, 9, "an out-of-range index is passed through as an id")

	named := []TorrentFile{file(7, "Show.S02E05.mkv", 100), file(8, "Show.S02E06.mkv", 100)}
	got, err = selectFileID(named, ep(2, 6))
	wantPick(t, got, err, 8, "the name match wins over any index")

	// The indexer's fileIdx is not a second opinion once the file list is in hand: the list itself proves
	// S02E09 is not here, so an in-range index must not smuggle S02E05 back in.
	withIdx := ResolveTarget{InfoHash: H, Season: intp(2), Episode: intp(9), FileIdx: idx(0)}
	got, err = selectFileID(named, withIdx)
	wantNotInTorrent(t, got, err, "a fileIdx cannot override a proven mismatch")
}

// Real-Debrid: same name-match precedence, but an unusable index falls back to the FIRST file rather
// than to nothing — RD always needs a concrete file id to unrestrict.
func TestPickFileID_realDebrid(t *testing.T) {
	s := &realDebridStore{}
	files := []TorrentFile{file(3, "a.mkv", 10), file(4, "b.mkv", 20)}

	if got, err := s.pickFileID(nil, ep(1, 1)); got != nil || err != nil {
		t.Errorf("no files → no pick: got %v, %v", got, err)
	}
	got, err := s.pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(1)})
	wantPick(t, got, err, 4, "in-range index")
	got, err = s.pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(99)})
	wantPick(t, got, err, 3, "out of range → first file")

	named := []TorrentFile{file(3, "Show.S01E04.mkv", 10), file(4, "Show.S01E05.mkv", 20)}
	got, err = s.pickFileID(named, ep(1, 5))
	wantPick(t, got, err, 4, "the name match wins")

	// Without the refusal, RD's own fallback ("first file") hands back S01E04 for a request for S01E09.
	got, err = s.pickFileID(named, ep(1, 9))
	wantNotInTorrent(t, got, err, "RD refuses rather than falling back to the first file")
}

// Premiumize: an unusable index falls back to the LARGEST file — with no id space of its own, the
// biggest file in a pack is the best guess at the feature.
func TestPickIndex_premiumize(t *testing.T) {
	s := &premiumizeStore{}
	files := []TorrentFile{file(0, "small.mkv", 10), file(1, "big.mkv", 9000)}

	if got, err := s.pickIndex(nil, ep(1, 1)); got != nil || err != nil {
		t.Errorf("no files → no pick: got %v, %v", got, err)
	}
	got, err := s.pickIndex(files, ResolveTarget{InfoHash: H, FileIdx: idx(0)})
	wantPick(t, got, err, 0, "in-range index")
	got, err = s.pickIndex(files, ResolveTarget{InfoHash: H, FileIdx: idx(42)})
	wantPick(t, got, err, 1, "out of range → largest")
	got, err = s.pickIndex(files, ResolveTarget{InfoHash: H})
	wantPick(t, got, err, 1, "no index at all → largest")

	named := []TorrentFile{file(0, "Show.S03E01.mkv", 10), file(1, "Show.S03E02.mkv", 20)}
	got, err = s.pickIndex(named, ep(3, 1))
	wantPick(t, got, err, 0, "the name match wins over the largest")

	// Premiumize's largest-file fallback is the most dangerous of the three: it reliably returns the
	// biggest episode in the pack, which looks entirely plausible right up until it plays.
	got, err = s.pickIndex(named, ep(3, 9))
	wantNotInTorrent(t, got, err, "Premiumize refuses rather than serving the largest episode")
}

// A negative index is untrusted input from an addon, not an offset — it must never index backwards.
func TestPickers_rejectNegativeIndex(t *testing.T) {
	files := []TorrentFile{file(0, "a.mkv", 10), file(1, "b.mkv", 20)}
	neg := ResolveTarget{InfoHash: H, FileIdx: idx(-1)}

	got, err := selectFileID(files, neg)
	wantPick(t, got, err, -1, "TorBox hands a negative back as an id, never as an offset")
	got, err = (&realDebridStore{}).pickFileID(files, neg)
	wantPick(t, got, err, 0, "RD falls back to the first file")
	got, err = (&premiumizeStore{}).pickIndex(files, neg)
	wantPick(t, got, err, 1, "Premiumize falls back to the largest")
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
