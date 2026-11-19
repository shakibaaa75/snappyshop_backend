package models

import (
	"regexp"
	"sync"
	"testing"
)

var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewUUIDv7Format(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if id := NewUUIDv7(); !uuidV7Re.MatchString(id) {
			t.Fatalf("not a valid UUIDv7: %q", id)
		}
	}
}

func TestNewUUIDv7IsStrictlyIncreasing(t *testing.T) {
	prev := NewUUIDv7()
	for i := 0; i < 20000; i++ { // far more than 4096 per ms: exercises the counter
		cur := NewUUIDv7()
		if cur <= prev {
			t.Fatalf("IDs must increase: %q then %q", prev, cur)
		}
		prev = cur
	}
}

func TestNewUUIDv7UniqueAcrossGoroutines(t *testing.T) {
	const workers, each = 16, 2000
	var mu sync.Mutex
	seen := make(map[string]struct{}, workers*each)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, each)
			for i := 0; i < each; i++ {
				local = append(local, NewUUIDv7())
			}
			mu.Lock()
			for _, id := range local {
				if _, dup := seen[id]; dup {
					t.Errorf("duplicate ID %q", id)
				}
				seen[id] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
}
