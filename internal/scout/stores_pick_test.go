package scout

import (
	"errors"
	"fmt"
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
	// An out-of-range index describes a file list that is not this one, so it is no opinion at all — and
	// files[0] handed back whatever sorted first, which on a release with a Sample/ directory is the
	// sample. The largest file is the same guess Premiumize makes, and what "no index at all" already got.
	got, err = s.pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(99)})
	wantPick(t, got, err, 4, "out of range → largest, not whatever sorted first")

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
	wantPick(t, got, err, 1, "RD falls back to the largest")
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

// The per-store fallbacks pick from the VIDEOS, the same pool pickEpisodeFile uses. RD's and Premiumize's
// tails picked over every file, which was unreachable while pickEpisodeFile answered for an unlabelled
// pack — and the moment it stopped, a pack whose biggest entry is an .iso resolved to that: a 302, no
// error, no video.
func TestStores_theFallbackNeverPicksANonVideo(t *testing.T) {
	files := []TorrentFile{
		file(0, "Show.S01.Extras.iso", 9000),
		file(1, "Show.1080p.mkv", 4000),
		file(2, "Sample/sample.mkv", 2),
	}
	for _, tc := range []struct {
		name string
		pick func(ResolveTarget) (*int, error)
		// TorBox is excluded from the out-of-range case on purpose: an index past the list is handed back
		// as a raw TorBox file id there, which is its own documented id space rather than a slice offset.
		outOfRangeFallsBack bool
	}{
		{"torbox", func(t ResolveTarget) (*int, error) { return selectFileID(files, t) }, false},
		{"realdebrid", func(t ResolveTarget) (*int, error) { return (&realDebridStore{}).pickFileID(files, t) }, true},
		{"premiumize", func(t ResolveTarget) (*int, error) { return (&premiumizeStore{}).pickIndex(files, t) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			targets := []ResolveTarget{{InfoHash: H, Season: intp(1), Episode: intp(3)}}
			if tc.outOfRangeFallsBack {
				targets = append(targets, ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(3), FileIdx: idx(42)})
			}
			for _, target := range targets {
				got, err := tc.pick(target)
				wantPick(t, got, err, 1, "the feature, not the .iso")
			}
		})
	}
	// A pool with no video at all still has to answer with something rather than nothing.
	onlyOther := []TorrentFile{file(0, "Show.iso", 10), file(1, "Show.big.iso", 9000)}
	got, err := (&realDebridStore{}).pickFileID(onlyOther, ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(3)})
	wantPick(t, got, err, 1, "with no video anywhere, the largest is all there is")
}

// A pack numbered the way most anime and much TV is packed: the episode is found by its own number.
//
// A miss, though, is NOT evidence the pack lacks the episode — bare numbering is often absolute, so
// season 2 episode 1 is packed as `- 13` and nothing in the name says so. Only an unambiguous SxxExx
// label, which carries its season, can condemn a pack; a bare miss defers to the indexer's file index.
func TestPickEpisodeFile_bareNumberedPacks(t *testing.T) {
	pack := []TorrentFile{
		file(0, "[Grp] Show - 01 [1080p].mkv", 900),
		file(1, "[Grp] Show - 02 [1080p].mkv", 4000), // the largest: what the old fallback served
		file(2, "[Grp] Show - 03 [1080p].mkv", 950),
	}
	got, err := pickEpisodeFile(pack, 1, 3)
	wantPick(t, got, err, 2, "the episode is named by its own number")
	got, err = pickEpisodeFile(pack, 1, 1)
	wantPick(t, got, err, 0, "and so is the first one")
	if got, err := pickEpisodeFile(pack, 1, 9); got != nil || err != nil {
		t.Errorf("a bare-number miss must defer, not condemn: got %v, %v", got, err)
	}
}

// The bare-number matcher's boundaries, asserted where they can actually fail.
//
// Every name below is a real release shape. A dot cannot open a standalone number and can only close one
// when no digit follows, because `DDP5.1`, `TrueHD.7.1`, `AC3.5.1`, `H.264`, `AAC.2.0` and a date all
// contain dot-delimited digit runs. Treating the dot as an ordinary separator made every file in a pack
// with 5.1 audio — most packs — match episode 1, and the largest was served: ask for E01 of a
// ten-episode pack, get E10, 302, no error.
func TestEpisodePatterns_bareNumberBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		episode int
		want    bool
		why     string
	}{
		{"[Grp] Show - 03 [1080p].mkv", 3, true, "the shape the matcher exists for"},
		{"[Grp] Show - 3 [1080p].mkv", 3, true, "unpadded"},
		{"[Grp] Show - 03.mkv", 3, true, "a dot closes the token when no digit follows"},
		{"Show_07_1080p.mkv", 7, true, "underscores are separators too"},
		{"Show.S01E03.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv", 1, false, "DDP5.1 is not episode 1"},
		{"Show.S01E03.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv", 264, false, "H.264 is not episode 264"},
		{"Show.S03E07.TrueHD.7.1.Atmos-GRP.mkv", 1, false, "TrueHD.7.1 is not episode 1"},
		{"Show.S03E07.DTS-HD.MA.5.1-GRP.mkv", 5, false, "a dot-led 5 is not episode 5"},
		{"Show.S03E07.AAC.2.0-GRP.mkv", 2, false, "AAC.2.0 is not episode 2"},
		{"Show.S03E07.AAC.2.0-GRP.mkv", 0, false, "nor episode 0"},
		// Space-separated names are where the trailing rule earns its keep: the leading rule alone cannot
		// see `5` in "DDP 5.1", because a space really does open the token there. Only "a dot may not
		// close a number when a digit follows" rejects it.
		{"Show S03E07 DDP 5.1 Atmos-GRP.mkv", 5, false, "DDP 5.1 is not episode 5"},
		{"Show S03E07 DTS-HD MA 7.1-GRP.mkv", 7, false, "nor 7.1 episode 7"},
		{"Show.2024.03.15.1080p.WEB.h264-GRP.mkv", 3, false, "a date is not an episode"},
		{"[Group8] Feature [720p].mkv", 8, false, "a group tag is not an episode"},
		{"Movie.2019.1080p.BluRay.x264.mkv", 19, false, "a year is not an episode"},
		{"Show.S01E03.1080p.mkv", 1080, false, "a resolution is not an episode"},
		{"[Grp] Show - 03 [1080p][HEVC][10bit].mkv", 10, false, "10bit is not episode 10"},
	} {
		got := matchesEpisode(tc.name, bareEpisodePatterns(tc.episode))
		if got != tc.want {
			t.Errorf("ep %d vs %q: got %v, want %v — %s", tc.episode, tc.name, got, tc.want, tc.why)
		}
	}
}

// The two failures the boundaries above prevent, driven through the real pickers.
func TestPickEpisodeFile_modernPackNamesAreNotAllEpisodeOne(t *testing.T) {
	// A plain WEB-DL season pack. Every file carries DDP5.1 and H.264.
	pack := []TorrentFile{
		file(0, "Show.S01E01.First.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv", 900),
		file(1, "Show.S01E02.Second.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv", 950),
		file(2, "Show.S01E10.Tenth.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv", 9000),
	}
	got, err := pickEpisodeFile(pack, 1, 1)
	wantPick(t, got, err, 0, "E01 is E01, not the biggest file in the pack")

	// An absolute-numbered anime pack: season 2 packed as episodes 13-24, which is what the catalogue's
	// S02E01 means. The names match nothing, and the pool must NOT be condemned — the indexer's file
	// index is right and the caller needs the chance to use it.
	absolute := []TorrentFile{
		file(0, "[Grp] Show - 13 [1080p][HEVC][10bit].mkv", 900),
		file(1, "[Grp] Show - 14 [1080p][HEVC][10bit].mkv", 950),
	}
	if got, err := pickEpisodeFile(absolute, 2, 1); got != nil || err != nil {
		t.Errorf("an absolute-numbered pack must defer to the indexer: got %v, %v", got, err)
	}

	// A lone episode beside its sample, neither naming an episode: still playable.
	sampled := []TorrentFile{
		file(0, "Show.1080p.WEB-DL.DDP5.1.H.264-NTb.mkv", 4000),
		file(1, "Show.1080p.WEB-DL.DDP5.1.H.264-NTb-sample.mkv", 2),
	}
	got, err = selectFileID(sampled, ResolveTarget{InfoHash: H, Season: intp(1), Episode: intp(3)})
	wantPick(t, got, err, 0, "a feature plus a sample is not a pack that lacks your episode")
}

// The precedence between the two kinds of evidence, which a union got wrong.
//
// A bare number carries no season and cannot say which show it belongs to. Where the filenames DO name
// their episodes it is noise — `[10-bit]` is not episode 10, `SG-1` is not episode 1 — and unioning the
// two, then breaking the tie by file size, let that noise outrank the file that really did name the
// episode. Where the names carry no label it is the only evidence there is, and must still work.
func TestPickEpisodeFile_aBareNumberNeverOutranksALabel(t *testing.T) {
	// Every file is tagged [10-bit]; only one is E10. The tag must not win, and it is on the big file.
	tenBit := []TorrentFile{
		file(0, "Show.S01E10.1080p.WEB-DL.x265.[10-bit].mkv", 900),
		file(1, "Show.S01E11.1080p.WEB-DL.x265.[10-bit].mkv", 9000),
	}
	got, err := pickEpisodeFile(tenBit, 1, 10)
	wantPick(t, got, err, 0, "E10 is the file named E10, not the biggest one carrying a 10")

	// A number in the show's own title, on a properly labelled pack.
	titled := []TorrentFile{
		file(0, "Stargate SG-1 - S03E01 - Into the Fire.mkv", 900),
		file(1, "Stargate SG-1 - S03E02 - Seth.mkv", 9000),
	}
	got, err = pickEpisodeFile(titled, 3, 1)
	wantPick(t, got, err, 0, "SG-1 is the show, not episode 1")

	// And a labelled pack that lacks the episode is still refused, not answered from a title number.
	got, err = pickEpisodeFile(titled, 3, 9)
	wantNotInTorrent(t, got, err, "a labelled pack cannot be talked into an answer by a bare number")

	// Unlabelled, the bare number is all there is and must still decide.
	bare := []TorrentFile{
		file(0, "[Grp] Show - 01 [1080p].mkv", 9000),
		file(1, "[Grp] Show - 02 [1080p].mkv", 900),
	}
	got, err = pickEpisodeFile(bare, 1, 2)
	wantPick(t, got, err, 1, "with nothing else to go on, the bare number decides")
}

// Real-Debrid's in-range fileIdx branch maps a POSITION to RD's own file id. Nothing verified it: every
// fixture used ids equal to their slice positions, so the mapping and a raw pass-through were the same
// value and deleting the branch changed nothing. RD ids are 1-based in practice, and never a slice index.
func TestPickFileID_realDebridMapsPositionToItsOwnId(t *testing.T) {
	files := []TorrentFile{file(11, "a.mkv", 10), file(22, "b.mkv", 20), file(33, "c.mkv", 15)}
	got, err := (&realDebridStore{}).pickFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(2)})
	wantPick(t, got, err, 33, "position 2 is RD's file id 33, not the number 2")
}

// The same gap on the other two, so the three are pinned alike.
func TestPickers_mapPositionToTheStoresOwnId(t *testing.T) {
	files := []TorrentFile{file(11, "a.mkv", 10), file(22, "b.mkv", 20), file(33, "c.mkv", 15)}
	got, err := selectFileID(files, ResolveTarget{InfoHash: H, FileIdx: idx(1)})
	wantPick(t, got, err, 22, "TorBox: position 1 is its file id 22")
	// Premiumize indexes its own content array, so there the position IS the answer — asserted with ids
	// that differ from positions so the two cannot be confused.
	got, err = (&premiumizeStore{}).pickIndex(files, ResolveTarget{InfoHash: H, FileIdx: idx(1)})
	wantPick(t, got, err, 1, "Premiumize: the position is the answer, and it is not the id")
}

// All three stores hand back PATHS, so the torrent's root directory is part of every file's name — and
// no fixture in this suite was path-shaped, which is why the directory could speak for every file.
// A directory describes the pack; only the filename describes the file.
func TestPickEpisodeFile_theDirectoryIsNotEvidenceAboutTheFile(t *testing.T) {
	// A scene pack whose root names the whole range: every file matched a request for S01E01 through the
	// directory, and the largest was served.
	root := "Show.S01E01-E10.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb/"
	scene := []TorrentFile{
		file(201, root+"Show.S01E01.1080p.mkv", 900),
		file(208, root+"Show.S01E08.1080p.mkv", 9000),
	}
	got, err := pickEpisodeFile(scene, 1, 1)
	wantPick(t, got, err, 201, "E01 is the file named E01, not every file under a directory that says E01-E10")

	// An anime pack whose root carries a season and a bit-depth tag. `(Season 1)` and `[10-bit]` matched
	// episodes 1 and 10 in every file.
	anime := "[Anime Time] Show (Season 1) [BD 1080p][HEVC 10-bit][AAC]/"
	tagged := []TorrentFile{
		file(101, anime+"[Anime Time] Show - 01.mkv", 900),
		file(107, anime+"[Anime Time] Show - 07.mkv", 9000),
	}
	got, err = pickEpisodeFile(tagged, 1, 1)
	wantPick(t, got, err, 101, "episode 1 is the file numbered 01, not the one under a (Season 1) directory")
	if got, err := pickEpisodeFile(tagged, 1, 10); got != nil || err != nil {
		t.Errorf("[10-bit] in the directory is not episode 10: got %v, %v", got, err)
	}
	// Nested directories, which is what a season pack of a multi-season release looks like.
	deep := []TorrentFile{
		file(301, "Show Complete/Season 01/Show.S01E01.mkv", 900),
		file(308, "Show Complete/Season 01/Show.S01E08.mkv", 9000),
	}
	got, err = pickEpisodeFile(deep, 1, 1)
	wantPick(t, got, err, 301, "only the last component names the file")
}

// A number that matches EVERY candidate is part of what they share — the show's own title — not what
// tells them apart. Serving the largest of "all of them" is how a request for episode 1 of Stargate
// SG-1 returned episode 2.
func TestPickEpisodeFile_aTitleNumberIsNotAnEpisode(t *testing.T) {
	sg1 := []TorrentFile{
		file(301, "Stargate SG-1 - 01 - Children of the Gods.mkv", 900),
		file(302, "Stargate SG-1 - 02 - The Enemy Within.mkv", 9000),
	}
	// Every file matches episode 1 — one by its number, both by the title — so the match tells them apart
	// not at all, and the honest answer is to say nothing and let the indexer's own file index decide.
	// What must NOT happen is what did: taking the largest of "all of them" and serving episode 2 for a
	// request for episode 1.
	if got, err := pickEpisodeFile(sg1, 1, 1); got != nil || err != nil {
		t.Errorf("a number matching every file decides nothing: got %v, %v", got, err)
	}
	// Episode 2 is told apart by its own number, so it is still found.
	got, err := pickEpisodeFile(sg1, 1, 2)
	wantPick(t, got, err, 302, "episode 2 is named by one file only, so it is found")

	// Episode 7 is not here. "SG-1" must not answer for it, and neither must the largest file.
	blake := []TorrentFile{
		file(401, "Blake's 7 - 01 - The Way Back.mkv", 900),
		file(402, "Blake's 7 - 02 - Space Fall.mkv", 9000),
	}
	if got, err := pickEpisodeFile(blake, 1, 7); got != nil || err != nil {
		t.Errorf("a number in the title is not an episode: got %v, %v", got, err)
	}
}

// One labelled file must not switch the weak tier off for the rest of the pack. An anime batch commonly
// carries a single labelled special beside bare-numbered episodes; treating the pool as labelled turned
// a hit into a hard refusal, and the client records a release that plays as dead.
func TestPickEpisodeFile_oneLabelledFileDoesNotCondemnTheRest(t *testing.T) {
	batch := []TorrentFile{
		file(0, "[Grp] Show - 01 [1080p].mkv", 900),
		file(1, "[Grp] Show - 02 [1080p].mkv", 950),
		file(2, "[Grp] Show - 03 [1080p].mkv", 980),
		file(3, "[Grp] Show OVA S00E01 [1080p].mkv", 9000),
	}
	got, err := pickEpisodeFile(batch, 1, 3)
	wantPick(t, got, err, 2, "the special's label says nothing about the numbered episodes")

	// And when the bare tier finds nothing either, a mixed pack must DEFER rather than refuse — the
	// unlabelled files could be absolutely numbered, so their silence proves nothing about episode 9.
	if got, err := pickEpisodeFile(batch, 1, 9); got != nil || err != nil {
		t.Errorf("a mixed pack cannot be condemned by its one labelled file: got %v, %v", got, err)
	}

	// And a pack where EVERY file names its episode still refuses when none of them is yours.
	allLabelled := []TorrentFile{
		file(0, "Show.S01E01.mkv", 900),
		file(1, "Show.S01E02.mkv", 950),
	}
	got, err = pickEpisodeFile(allLabelled, 1, 9)
	wantNotInTorrent(t, got, err, "a fully labelled pack that lacks the episode holds nothing to play")
}

// One stray file cannot unmake a pack, and cannot rescue a number that decides nothing. Both guards used
// to be phrased as set-size comparisons over the pool, so a single extra video — a sample, a featurette,
// a bonus rip, all of them ordinary in a season pack and all of them videos — was enough to disarm each.
func TestPickEpisodeFile_oneStrayFileDisarmsNothing(t *testing.T) {
	labelled := []TorrentFile{
		file(0, "Show.S01E01.1080p.mkv", 900),
		file(1, "Show.S01E02.1080p.mkv", 950),
		file(2, "Show.S01E03.1080p.mkv", 980),
	}
	// The pack names its episodes and none of them is S02E01, so there is nothing here to play — and
	// that stays true with a sample, a featurette or both alongside.
	for _, extra := range [][]TorrentFile{
		nil,
		{file(3, "Sample/show-sample.mkv", 20)},
		{file(3, "Extras/Behind the Scenes.mkv", 9000)},
		{file(3, "Sample/show-sample.mkv", 20), file(4, "Extras/Behind the Scenes.mkv", 9000)},
	} {
		got, err := pickEpisodeFile(append(append([]TorrentFile{}, labelled...), extra...), 2, 1)
		wantNotInTorrent(t, got, err, fmt.Sprintf("a pack of other episodes plus %d extras", len(extra)))
	}

	// The mirror: a title number matches several files, so it tells them apart not at all — and adding a
	// sample must not turn that back into an answer.
	sg1 := []TorrentFile{
		file(301, "Stargate SG-1 - 01 - Children of the Gods.mkv", 900),
		file(302, "Stargate SG-1 - 02 - The Enemy Within.mkv", 950),
		file(303, "Stargate SG-1 - 03 - Emancipation.mkv", 9000),
	}
	withSample := append(append([]TorrentFile{}, sg1...), file(304, "sample.mkv", 20))
	for _, pool := range [][]TorrentFile{sg1, withSample} {
		if got, err := pickEpisodeFile(pool, 1, 1); got != nil || err != nil {
			t.Errorf("a number matching several files decides nothing (%d files): got %v, %v",
				len(pool), got, err)
		}
	}
	// And the episode whose number really is unique to one file is still found, sample or not.
	for _, pool := range [][]TorrentFile{sg1, withSample} {
		got, err := pickEpisodeFile(pool, 1, 2)
		wantPick(t, got, err, 302, "episode 2 is named by exactly one file")
	}

	// A single labelled episode is not a pack: it cannot condemn anything.
	one := []TorrentFile{file(0, "Show.S01E01.1080p.mkv", 900)}
	if got, err := pickEpisodeFile(one, 2, 1); got == nil || err != nil {
		t.Errorf("a lone episode file is not a pack that lacks your episode: got %v, %v", got, err)
	}
}

// A backslash is legal in a POSIX filename, and these names come from the torrent. Splitting on it
// unconditionally threw away the part of the name that carries the label.
func TestBaseName_onlySplitsWhereADirectoryReallyEnds(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Show.S01E01.mkv", "Show.S01E01.mkv"},
		{"Pack/Show.S01E01.mkv", "Show.S01E01.mkv"},
		{"Pack/Sub/Show.S01E01.mkv", "Show.S01E01.mkv"},
		// A backslash is a legal filename character and every client joins the torrent's path components
		// with a forward slash, so a backslash is never a separator here.
		{`Show S01E01 - Part 1 \ Part 2.mkv`, `Show S01E01 - Part 1 \ Part 2.mkv`},
		{`Pack/Show S01E01 - Part 1 \ Part 2.mkv`, `Show S01E01 - Part 1 \ Part 2.mkv`},
	} {
		if got := baseName(tc.in); got != tc.want {
			t.Errorf("baseName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The label survives, which is the point.
	backslashed := []TorrentFile{
		file(0, `Show S01E01 - Part 1 \ Part 2.mkv`, 900),
		file(1, `Show S01E02 - Part 1 \ Part 2.mkv`, 9000),
	}
	got, err := pickEpisodeFile(backslashed, 1, 1)
	wantPick(t, got, err, 0, "a backslash in the filename does not erase its episode label")
}
