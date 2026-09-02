package scout

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dlclark/regexp2"
)

// Season-pack → episode file-index map (SCOUT-05, ported from src/season.ts). The episode patterns
// use negative lookahead → regexp2 (RE2 can't); they're trusted (numbers only), not user input.

// TorrentFile is one file in a torrent's list (index = the debrid store's file id).
type TorrentFile struct {
	Index     int
	Name      string
	SizeBytes *int
}

var videoExtRe = mustRE2(`\.(mkv|mp4|avi|m4v|ts|mov|wmv|flv|webm)$`)

// episodePatterns builds the SxxExx / 1x02 / "season 1 episode 2" / 102 / 0102 matchers once per pick.
// These NAME the episode: each carries the season with it, so a match is a statement about the file.
func episodePatterns(season, episode int) []*regexp2.Regexp {
	return compileAll(
		fmt.Sprintf(`s0*%d[ ._-]*e0*%d(?!\d)`, season, episode),
		fmt.Sprintf(`\b0*%dx0*%d(?!\d)`, season, episode),
		fmt.Sprintf(`season[ ._-]*0*%d[ ._-]*episode[ ._-]*0*%d(?!\d)`, season, episode),
		// Bare concatenated forms (102 = S1E02) must not swallow a resolution token: a trailing p/i
		// makes "720" (S7E20) match "720p", so exclude those as well as a following digit.
		fmt.Sprintf(`\b%d%02d(?![\dpi])`, season, episode),
		fmt.Sprintf(`\b%02d%02d(?![\dpi])`, season, episode),
	)
}

// bareEpisodePatterns is the episode number standing alone as its own token: `[Grp] Show - 03
// [1080p].mkv`, which is how most anime and a good deal of TV is packed. The delimiters must be real
// separators rather than "any non-digit", or `[Group8]` matches episode 8, and the token must be the
// WHOLE digit run, which keeps 1080p, 720p, x264 and a year out.
//
// A DOT CANNOT OPEN THE TOKEN, and can only close it when a digit does not follow. Both rules exist for
// the same family of names: `DDP5.1`, `TrueHD.7.1`, `AC3.5.1`, `H.264`, `AAC.2.0`, `2024.03.15`. With
// the dot an ordinary separator, every one of those is a standalone number, so on any pack with 5.1
// audio — which is most of them — EVERY file matched episode 1 and the largest was served.
//
// It is kept SEPARATE, and consulted only when nothing above matched, because it carries no season and
// cannot say which show a number belongs to. Unioned with the patterns above and resolved by file size,
// a number in the title (`Blake's 7`, `SG-1`) or a tag like `[10-bit]` outranked the file that really
// did name the episode — the strong evidence losing to the weak, on the largest file.
func bareEpisodePatterns(episode int) []*regexp2.Regexp {
	return compileAll(fmt.Sprintf(`(?:^|[ _\-\[\]()])0*%d(?:[ _\-\[\]()]|\.(?!\d)|$)`, episode))
}

func compileAll(specs ...string) []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, 0, len(specs))
	for _, s := range specs {
		if re, err := regexp2.Compile(s, regexp2.None); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func matchesEpisode(name string, patterns []*regexp2.Regexp) bool {
	n := strings.ToLower(baseName(name))
	for _, re := range patterns {
		if ok, _ := re.MatchString(n); ok {
			return true
		}
	}
	return false
}

// baseName drops the directories. All three stores hand back PATHS — TorBox's `name`, RD's `path`,
// Premiumize's `path` — so the torrent's root directory is part of every file's name, and matching the
// whole thing let one directory speak for every file in the pack. A scene pack rooted at
// `Show.S01E01-E10.1080p…/` matched a request for S01E01 in all ten files and served the largest; an
// anime pack rooted at `[Grp] Show (Season 1) [BD 1080p][HEVC 10-bit]/` did the same for episodes 1 and
// 10. In both cases a correct fileIdx was discarded on the way past. A directory describes the pack; only
// the filename describes the file.
func baseName(name string) string {
	// `/` only. The BitTorrent path is a list of components and every client joins it with a forward
	// slash, which is what all three debrid APIs hand back. A backslash, meanwhile, is a legal character
	// in a POSIX filename, and nothing in the string says which one it is: splitting on it turned
	// `Show S01E01 - Part 1 \ Part 2.mkv` into ` Part 2.mkv`, losing the label and reclassifying a
	// perfectly well-named file as unlabelled.
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

// errEpisodeNotInTorrent — the file list is episode-labelled and holds no file for the episode asked
// for. The release is a pack of OTHER episodes, so there is nothing here to play.
var errEpisodeNotInTorrent = errors.New("torrent holds no file for that episode")

// labelledEpisodeRe — does a filename DECLARE an episode, in a form that can't be anything else?
//
// Deliberately narrower than episodePatterns: the bare concatenated forms there (102, 0102) are fine for
// confirming an episode you already expect, but useless as evidence that a name is episode-labelled at
// all — "Movie.2019.1080p" is full of three- and four-digit runs. Only the unambiguous forms count, so
// the mismatch below is claimed only when a filename really does name a different episode.
var labelledEpisodeRe = mustRE2(`(s\d{1,2}[ ._-]*e\d{1,3}|\b\d{1,2}x\d{2}\b|season[ ._-]*\d{1,2}[ ._-]*episode[ ._-]*\d{1,3})`)

// pickEpisodeFile picks the file index for an episode: an SxxExx match among video files (largest on
// ties), else the largest video file.
//
// Returns errEpisodeNotInTorrent when nothing matched but the pool IS episode-labelled. Falling back to
// the largest file there serves a confidently wrong episode: the pack holds S01E01–E10, you asked for
// E11, and it hands back E07 as though that were the answer. A single-episode release whose filename
// carries no marker still falls back, because there the largest file is the only thing it could be.
func pickEpisodeFile(files []TorrentFile, season, episode int) (*int, error) {
	pool := episodeFilePool(files)
	if len(pool) == 0 {
		return nil, nil
	}

	patterns := episodePatterns(season, episode)
	var matched, unlabelled []TorrentFile
	for _, f := range pool {
		if matchesEpisode(f.Name, patterns) {
			matched = append(matched, f)
		}
		// Per FILE, not per pool. A pool-wide OR let one labelled file speak for the rest: an anime batch
		// carrying a single `S00E01` special alongside bare-numbered episodes had the weak tier switched
		// off for all of them, and the miss then became a hard refusal — a release that plays, answered
		// 404 and recorded dead by the client.
		if !labelledEpisodeRe.match(strings.ToLower(baseName(f.Name))) {
			unlabelled = append(unlabelled, f)
		}
	}
	if len(matched) > 0 {
		idx := largest(matched).Index
		return &idx, nil
	}
	// Only now the weak evidence, and only against the files that do NOT name their own episode. A bare
	// number carries no season and cannot say which show it belongs to, so on a file already labelled
	// S01E05 it is noise; on one labelled nothing it is all there is.
	if len(unlabelled) > 0 {
		bareRe := bareEpisodePatterns(episode)
		var bare []TorrentFile
		for _, f := range unlabelled {
			if matchesEpisode(f.Name, bareRe) {
				bare = append(bare, f)
			}
		}
		// Exactly one, or it is not evidence. In a numbered pack the episode's own number appears in one
		// file; a number that turns up in several is something they share — the show's title — and
		// picking the biggest of those served `Stargate SG-1 - 03` for a request for episode 1.
		//
		// The test used to be "did it match EVERY candidate", which asked about set sizes rather than
		// about whether the evidence tells the files apart, and one odd file was enough to disarm it: add
		// a sample to the same SG-1 pack and 3 of 4 matched, so the equality failed and the largest was
		// served again. Counting to one has no such seam.
		if len(bare) == 1 {
			idx := bare[0].Index
			return &idx, nil
		}
	}
	// A pack that NAMES its episodes, none of them yours, holds nothing to play. Two labelled files are
	// what make it a pack: one is a single episode, and a bare number can never prove absence — bare
	// numbering is frequently ABSOLUTE, so season 2 episode 1 is packed as `[Grp] Show - 13` and nothing
	// in the name says so, so refusing on that would kill every episode of an ordinary anime season.
	//
	// It counts the LABELLED files rather than requiring every file to be labelled. The stricter test
	// asked about the pool instead of about the pack, so a single unlabelled extra — a sample, a
	// featurette, a bonus rip, all of which are videos and all of which are ordinary in a season pack —
	// disarmed the refusal for every episode of that pack, and the stores then served their largest
	// video: a 302, no error, the wrong episode. One stray file cannot unmake a pack.
	if len(pool)-len(unlabelled) > 1 {
		return nil, errEpisodeNotInTorrent
	}
	// No opinion, rather than a guess. The fallback below is sound only for a pool of ONE, where "the
	// largest file is the only thing it could be" is literally true. For a multi-file pack it hands back
	// the biggest episode as though it were the one asked for — and because a non-nil answer makes every
	// caller return early, it also DISCARDS the indexer's fileIdx, which is a position in this very
	// torrent and was right. That is the whole of `[Grp] Show - 03 [1080p].mkv`: a bare episode number is
	// not one of the unambiguous forms labelledEpisodeRe accepts — it cannot be, "Movie.2019.1080p" is
	// full of digit runs — so a standard anime/TV pack was judged unlabelled and every episode of it
	// resolved to the same file, with a 302 and no error.
	if len(pool) > 1 {
		return nil, nil
	}
	idx := largest(pool).Index
	return &idx, nil
}

// episodeFilePool — the files an episode may be picked from: the videos, or everything when a list
// carries no recognised video extension.
func episodeFilePool(files []TorrentFile) []TorrentFile {
	var videos []TorrentFile
	for _, f := range files {
		if videoExtRe.match(strings.ToLower(f.Name)) {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		return files
	}
	return videos
}

func largest(files []TorrentFile) TorrentFile {
	best := files[0]
	for _, f := range files[1:] {
		if intOr(f.SizeBytes, 0) > intOr(best.SizeBytes, 0) {
			best = f
		}
	}
	return best
}

// largestPlayable is largest() over the files that could be the feature — the same video-only pool
// pickEpisodeFile picks from, so a store falling back on its own reaches the same kind of answer. Picking
// over every file instead served a pack's `.iso` or `.rar` as the episode: a 302, no error, no video.
func largestPlayable(files []TorrentFile) TorrentFile {
	if pool := episodeFilePool(files); len(pool) > 0 {
		return largest(pool)
	}
	return largest(files)
}
