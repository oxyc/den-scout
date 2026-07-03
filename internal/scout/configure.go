package scout

import _ "embed"

// The /configure page — embedded so the binary is self-contained (distroless, no runtime file).
// Builds the install link in the browser and seals the debrid token to the addon's key
// (den-scout/docs/SEALED-CONFIG.md); shares the layout + DenSeal bundle with den-subtitles/den-reel.
//
//go:embed configure.html
var configurePage string
