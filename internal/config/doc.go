// Package config loads, validates and persists the runtime configuration of the bot
// (TOML format).
//
// # Design decision: the write-back tradeoff
//
// The go-toml/v2 Marshal drops every comment (known upstream limitation, no fix planned).
// The strategy of this package is:
//   - the config file is treated as read-only input: read once at startup, never written
//     back while the process runs.
//   - mutable runtime state (task list, session, etc.) is stored separately by
//     internal/state.
//   - the [Save] function is only for generating the default config on first run, or as a
//     test helper; **it drops every comment** and must not be used to save a
//     hand-edited config.
//
// This differs from WriteDefault, which generates a commented default config from a
// hard-coded template string and never passes through toml.Marshal, so the comments stay
// intact.
//
// Precedence (low to high): built-in defaults → TOML file → environment variables.
package config
