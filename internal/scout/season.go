package scout

import (
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
		fmt.Sprintf(`\b%d%02d(?!\d)`, season, episode),
		fmt.Sprintf(`\b%02d%02d(?!\d)`, season, episode),
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

// pickEpisodeFile picks the file index for an episode: an SxxExx match among video files (largest on
// ties), else the largest video file; nil only when there's no file at all.
func pickEpisodeFile(files []TorrentFile, season, episode int) *int {
	var videos []TorrentFile
	for _, f := range files {
		if videoExtRe.match(strings.ToLower(f.Name)) {
			videos = append(videos, f)
		}
	}
	pool := videos
	if len(pool) == 0 {
		pool = files
	}
	if len(pool) == 0 {
		return nil
	}

	patterns := episodePatterns(season, episode)
	var matched []TorrentFile
	for _, f := range pool {
		if matchesEpisode(f.Name, patterns) {
			matched = append(matched, f)
		}
	}
	if len(matched) > 0 {
		idx := largest(matched).Index
		return &idx
	}
	idx := largest(pool).Index
	return &idx
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
