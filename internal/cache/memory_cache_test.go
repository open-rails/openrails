package cache

import (
	"sync"
	"testing"
)

func TestMemoryCacheCloseJoinsCleanup(t *testing.T) {
	c := NewMemoryCache()
	var closers sync.WaitGroup
	for range 8 {
		closers.Go(func() {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
			select {
			case <-c.done:
			default:
				t.Error("Close returned before cleanup exited")
			}
		})
	}
	closers.Wait()
}
