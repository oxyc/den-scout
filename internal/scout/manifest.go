package scout

import "strings"

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
}

type manifestHints struct {
	Configurable          bool `json:"configurable"`
	ConfigurationRequired bool `json:"configurationRequired"`
}

const manifestVersion = "0.1.0"

// buildManifest returns the manifest for a config (nil = unconfigured).
func buildManifest(config *Config) manifestJSON {
	configured := config != nil
	description := "Self-hosted stream aggregator for Den — configure to add your debrid token."
	if configured {
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
	}
}
