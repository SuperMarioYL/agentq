// Package agentq exposes the embedded web triage UI as a single
// embed.FS so the daemon ships as one static binary. Go's embed
// directive cannot cross the .. boundary, so this file lives at the
// module root next to the web/ tree it captures.
package agentq

import "embed"

//go:embed web
var WebFS embed.FS
