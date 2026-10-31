//go:build headless

package consoleassets

import "embed"

// Assets is empty in the inference-only binary: no console files are embedded.
var Assets embed.FS
