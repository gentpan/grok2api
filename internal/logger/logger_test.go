package logger

import (
	"sync"
	"testing"
)

// Run with -race: logf must not read level without the lock while Reload writes it.
func TestLogfConcurrentWithReload(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				Debugf("noise %d", j)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if j%2 == 0 {
					Reload("ERROR", "", 0)
				} else {
					Reload("DEBUG", "", 0)
				}
			}
		}()
	}
	wg.Wait()
	Reload("INFO", "", 0)
}
