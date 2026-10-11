package scout

import (
	"os"
	"strings"
)

// Stremio manifest (SCOUT-01, ported from src/manifest.ts). Field order + values match the golden.
type manifestJSON struct {
	ID            string        `json:"id"`
	Version       string        `json:"version"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Resources     []string      `json:"resources"`
	Types         []string      `json:"types"`
	IDPrefixes    []string      `json:"idPrefixes"`
	Catalogs      []struct{}    `json:"catalogs"`
	BehaviorHints manifestHints `json:"behaviorHints"`
	// DenInstallID is the configured install's id, spelled as REVOKED_INSTALLS takes it, so the owner can
	// revoke one link from what the app shows. Omitted when the config carries none.
	DenInstallID string `json:"denInstallId,omitempty"`
}

type manifestHints struct {
	Configurable          bool `json:"configurable"`
	ConfigurationRequired bool `json:"configurationRequired"`
}

// devVersion is what a binary reports when nothing told it which release it is: `go run`, tests, a plain
// `docker build`. Release builds never see it — the Dockerfile refuses an image without DEN_VERSION.
const devVersion = "0.0.0-dev"

// manifestVersion is the release this process reports in the manifest, /metrics and the startup line. It
// comes from DEN_VERSION, which the image carries from the VERSION file, rather than from a constant in the
// source: a constant changes the compiled package on every release, which recompiled the binary for a
// release that changed nothing else.
var manifestVersion = versionFrom(os.Getenv)

func versionFrom(get func(string) string) string {
	if v := get("DEN_VERSION"); v != "" {
		return v
	}
	return devVersion
}

// buildManifest returns the manifest for a config (nil = unconfigured).
func buildManifest(config *Config) manifestJSON {
	configured := config != nil
	description := "Self-hosted stream aggregator for Den — configure to add your debrid token."
	installID := ""
	if configured {
		installID = config.IID
		services := make([]string, len(config.Debrid))
		for i, d := range config.Debrid {
			services[i] = string(d.Service)
		}
		description = "Cached-first, ranked, junk-filtered streams via " + strings.Join(services, ", ") + "."
	}
	return manifestJSON{
		ID:            "com.den.scout",
		Version:       manifestVersion,
		Name:          "Den Scout",
		Description:   description,
		Resources:     []string{"stream"},
		Types:         []string{"movie", "series"},
		IDPrefixes:    []string{"tt"},
		Catalogs:      []struct{}{},
		BehaviorHints: manifestHints{Configurable: true, ConfigurationRequired: !configured},
		DenInstallID:  installID,
	}
}
