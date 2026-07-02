package scout

import _ "embed"

// The /configure page — embedded so the binary is self-contained (distroless, no runtime file).
// Byte-identical to the TS CONFIGURE_PAGE (captured golden).
//
//go:embed configure.html
var configurePage string
