package policy

import (
	"testing"

	"ferry/internal/domain"
)

func TestFirstOutputTimeoutSnapshotAndHash(t *testing.T) {
	compiler := NewCompiler()
	hashes := map[string]int{}
	for _, seconds := range []int{0, 1, 180, 7200} {
		file := stableHashV2Config("test")
		file.LogicalModels[0].FirstOutputTimeoutSeconds = seconds
		file.LogicalModels = append(file.LogicalModels, domain.LogicalModel{ID: "other", FirstOutputTimeoutSeconds: 30})
		snapshot, err := compiler.CompileAtRevision(file, 23)
		if err != nil {
			t.Fatal(err)
		}
		if got := snapshot.LogicalModels["smart"].FirstOutputTimeoutSeconds; got != seconds {
			t.Fatalf("compiled timeout=%d want=%d", got, seconds)
		}
		if previous, exists := hashes[snapshot.Hash]; exists {
			t.Fatalf("timeouts %d and %d shared configuration hash %s", previous, seconds, snapshot.Hash)
		}
		hashes[snapshot.Hash] = seconds
		republished, err := compiler.CompileAtRevision(file, 23)
		if err != nil || republished.Hash != snapshot.Hash {
			t.Fatalf("republishing timeout=%d changed hash: %v", seconds, err)
		}
		file.LogicalModels[0].FirstOutputTimeoutSeconds = 42
		if got := snapshot.LogicalModels["smart"].FirstOutputTimeoutSeconds; got != seconds {
			t.Fatalf("source config mutation changed admitted snapshot: got=%d want=%d", got, seconds)
		}
		if got := snapshot.LogicalModels["other"].FirstOutputTimeoutSeconds; got != 30 {
			t.Fatalf("timeout leaked across logical models: %d", got)
		}
	}
}
