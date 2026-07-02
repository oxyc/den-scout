package scout

import (
	"fmt"
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
