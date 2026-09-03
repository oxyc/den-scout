package scout

import (
	"fmt"
	"testing"
)

// Episode selection is the hottest pure-CPU path in the service: it runs once per file, per pick, and
// every matching bug fixed here has added another pattern. Nothing measured it, so the cost of compiling
// those patterns per FILE rather than per PICK went unnoticed until it was 10x — see episodeNamer.
func benchPack(n int, shape string) []TorrentFile {
	var out []TorrentFile
	for i := 1; i <= n; i++ {
		sz := 1000 + i
		out = append(out, TorrentFile{Index: i, Name: fmt.Sprintf(shape, i), SizeBytes: &sz})
	}
	return out
}

// The shape a real season pack has: every file dot-separated with audio/codec tags.
const scene = "Show.S01E%02d.1080p.AMZN.WEB-DL.DDP5.1.H.264-NTb.mkv"
const anime = "[Grp] Show - %02d [1080p][HEVC][10bit].mkv"

func BenchmarkPickEpisode_scene24(b *testing.B) {
	files := benchPack(24, scene)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pickEpisodeFile(files, 1, 13)
	}
}

func BenchmarkPickEpisode_anime24(b *testing.B) {
	files := benchPack(24, anime)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pickEpisodeFile(files, 1, 13)
	}
}

func BenchmarkPickEpisode_scene200(b *testing.B) {
	files := benchPack(200, scene)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pickEpisodeFile(files, 1, 199)
	}
}

// A whole season resolved one episode at a time — what a binge actually costs.
func BenchmarkPickEpisode_wholeSeason(b *testing.B) {
	files := benchPack(24, scene)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for ep := 1; ep <= 24; ep++ {
			_, _ = pickEpisodeFile(files, 1, ep)
		}
	}
}
