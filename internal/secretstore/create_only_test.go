package secretstore

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSetIfAbsentConcurrent(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		var s *Store
		var e error
		p := filepath.Join(t.TempDir(), "store")
		if sqlite {
			s, e = OpenSQLite(p)
		} else {
			s, e = Open(p)
		}
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		var count atomic.Int32
		var wg sync.WaitGroup
		for range 12 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.SetIfAbsent("key", "original") == nil {
					count.Add(1)
				}
			}()
		}
		wg.Wait()
		if count.Load() != 1 {
			t.Fatal("create was not exclusive")
		}
		if s.SetIfAbsent("key", "replacement") == nil {
			t.Fatal("replaced existing secret")
		}
		v, _ := s.Get("key")
		if v != "original" {
			t.Fatal("secret changed")
		}
	}
}
