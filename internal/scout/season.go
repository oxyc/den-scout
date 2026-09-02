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
func episodePatterns(season, episode int) []*regexp2.Regexp {
	specs := []string{
		fmt.Sprintf(`s0*%d[ ._-]*e0*%d(?!\d)`, season, episode),
		fmt.Sprintf(`\b0*%dx0*%d(?!\d)`, season, episode),
		fmt.Sprintf(`season[ ._-]*0*%d[ ._-]*episode[ ._-]*0*%d(?!\d)`, season, episode),
		// Bare concatenated forms (102 = S1E02) must not swallow a resolution token: a trailing p/i
		// makes "720" (S7E20) match "720p", so exclude those as well as a following digit.
		fmt.Sprintf(`\b%d%02d(?![\dpi])`, season, episode),
		fmt.Sprintf(`\b%02d%02d(?![\dpi])`, season, episode),
	}
	out := make([]*regexp2.Regexp, 0, len(specs))
	for _, s := range specs {
		if re, err := regexp2.Compile(s, regexp2.None); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func matchesEpisode(name string, patterns []*regexp2.Regexp) bool {
	n := strings.ToLower(name)
	for _, re := range patterns {
		if ok, _ := re.MatchString(n); ok {
			return true
		}
	}
	return false
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
	var matched []TorrentFile
	labelled := false
	for _, f := range pool {
		if matchesEpisode(f.Name, patterns) {
			matched = append(matched, f)
		}
		if labelledEpisodeRe.match(strings.ToLower(f.Name)) {
			labelled = true
		}
	}
	if len(matched) > 0 {
		idx := largest(matched).Index
		return &idx, nil
	}
	if labelled {
		return nil, errEpisodeNotInTorrent
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
