package scout

import (
	"fmt"
	"regexp"
	"strings"
)

// Clean stream labels (SCOUT-03, ported from src/label.ts). Reuses rank.go's package-level regexes.

const mib = 1_048_576

var reHDRLabel = mustRE2(`\bhdr\b|hdr10|\bhlg\b`)

func resolutionLabel(t string, sizeBytes *int) string {
	switch {
	case res2160.match(t):
		return "4K"
	case res1440.match(t):
		return "1440p"
	case res1080.match(t):
		return "1080p"
	case res720.match(t):
		return "720p"
	case res480.match(t) || res576.match(t) || res540.match(t):
		return "SD"
	}
	if res4kUHD.match(t) && intOr(sizeBytes, 0) > 3*gib {
		return "4K"
	}
	return ""
}

func dynamicRangeLabel(t string) string {
	switch {
	case reDoVi.match(t):
		return "Dolby Vision"
	case reHDR10p.match(t):
		return "HDR10+"
	case reHDRLabel.match(t):
		return "HDR"
	}
	return ""
}

func sourceLabelForDisplay(t string) string {
	switch {
	case reRemux.match(t):
		return "REMUX"
	case reBluray.match(t) || reBrRip.match(t):
		return "BluRay"
	case reWebDL.match(t):
		return "WEB-DL"
	case reWebRip.match(t) || reWeb.match(t):
		return "WEB"
	}
	return ""
}

// sizeLabel: bytes → "18 GB" / "720 MB" (one decimal under 10 GB so small files stay legible).
func sizeLabel(bytes int) string {
	if bytes >= gib {
		gb := float64(bytes) / float64(gib)
		if gb >= 10 {
			return fmt.Sprintf("%d GB", int(gb+0.5))
		}
		return fmt.Sprintf("%.1f GB", gb)
	}
	mb := int(float64(bytes)/float64(mib) + 0.5)
	if mb < 1 {
		mb = 1
	}
	return fmt.Sprintf("%d MB", mb)
}

// bingeGroup names the release a stream belongs to, for next-episode continuity: a client plays the next
// episode from the source whose bingeGroup matches the one it just played. It used to be
// `den-scout-<imdb>` — the same for every release of a title — so the first candidate always matched and
// "stay on the same release" never happened. Resolution, source, dynamic range and release group are
// what make two episodes "the same release".
func bingeGroup(t, title string) string {
	dr := ""
	switch {
	case reDoVi.match(t):
		dr = "dv"
	case reHDRLabel.match(t) || reHDR10p.match(t):
		dr = "hdr"
	}
	return strings.Join([]string{"den-scout", detectResolutionLower(t), detectSourceAttr(t), dr,
		releaseGroup(title)}, "|")
}

var (
	fileExt = regexp.MustCompile(`(?i)\.[a-z0-9]{2,4}$`)
	// A name that ends on one of these ends on metadata, not on a group ("…-Pilot-1080p.x265").
	groupMetadata = []string{"1080p", "2160p", "720p", "480p", "4k", "x264", "x265", "h264", "h265",
		"hevc", "avc", "av1", "webrip", "web-dl", "webdl", "bluray", "hdtv", "aac", "dts", "ddp", "eac3", "atmos"}
)

// releaseGroup is the scene group — the token after the final hyphen, before the extension — lowercased,
// or "" when the name doesn't end on one. Mirrors the app's parser in StreamSelectionView.
func releaseGroup(title string) string {
	name, _, _ := strings.Cut(title, "\n")
	base := fileExt.ReplaceAllString(strings.TrimSpace(name), "")
	dash := strings.LastIndex(base, "-")
	// A spaced " - " is a title separator ("Show - S01E01 - Pilot"), not a group.
	if dash <= 0 || base[dash-1] == ' ' {
		return ""
	}
	tail := strings.TrimSpace(base[dash+1:])
	lower := strings.ToLower(tail)
	if tail == "" || len(tail) > 14 || strings.Contains(tail, " ") || strings.Contains(lower, "www") {
		return ""
	}
	lowerBase := strings.ToLower(base)
	for _, m := range groupMetadata {
		// The suffix test catches a hyphenated token straddling the last dash: "…1080p WEB-DL" ends on
		// metadata, and its "group" would otherwise be "dl".
		if strings.Contains(lower, m) || strings.HasSuffix(lowerBase, m) {
			return ""
		}
	}
	return lower
}

// cleanLabel is the bullet-joined quality summary shown as attributes.label.
func cleanLabel(s RawStream) string { return cleanLabelLower(strings.ToLower(s.Title), s) }

// cleanLabelLower is cleanLabel when the caller already has the lowercased title.
func cleanLabelLower(t string, s RawStream) string {
	var parts []string
	if res := resolutionLabel(t, s.SizeBytes); res != "" {
		parts = append(parts, res)
	}
	if src := sourceLabelForDisplay(t); src != "" {
		parts = append(parts, src)
	}
	if dr := dynamicRangeLabel(t); dr != "" {
		parts = append(parts, dr)
	}
	if reAtmos.match(t) {
		parts = append(parts, "Atmos")
	}
	if s.SizeBytes != nil {
		parts = append(parts, sizeLabel(*s.SizeBytes))
	}
	if len(parts) == 0 {
		return "Stream"
	}
	return strings.Join(parts, " • ")
}
