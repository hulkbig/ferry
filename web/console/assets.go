//go:build !headless

// Package consoleassets exposes the reviewed production console assets to the embedded
// management server. The same files remain directly runnable as the mock
// product demo, so production and handoff cannot silently drift apart.
package consoleassets

import "embed"

// Assets contains the dependency-free console shell, adapter and fragments.
//
//go:embed import.html import.js import.css index.html brand.svg brand-reverse.svg favicon.svg app-loader.js api.js mock-data.js app.part*.jsfrag styles.part*.css
var Assets embed.FS
