// Package naming provides a safe pipeline for naming downloaded files: template
// rendering, filename sanitisation, path traversal protection and atomic name
// deduplication.
//
// Background: using a rendered template result directly as a write path is a common
// attack surface, including {{ .FileName }} containing a "../.." traversal, absolute
// paths, control characters, Windows reserved names and the like.
// This package closes each one: Render only produces a "relative path fragment", the
// caller must pass it through Sanitize, then confine it to the download root with
// SafeJoin, and finally reserve a name atomically with UniquePath.
//
// Constraint: this package must not import internal/tg (internal/tg depends on this
// package in reverse, which would form an import cycle).
package naming
