//go:build headless

package consoleassets

import "testing"

func TestHeadlessBuildDoesNotEmbedConsole(t *testing.T) {
	if _, err := Assets.ReadFile("index.html"); err == nil {
		t.Fatal("headless binary embeds UI")
	}
}
