package runtime

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadLockRegistryRetainsWaitersAndNormalizesKeys(t *testing.T) {
	var registry uploadLockRegistry
	release := registry.lock(" upload ")
	acquired, finish := make(chan struct{}), make(chan struct{})
	action := runUploadAction(func() error {
		unlock := registry.lock("upload")
		defer unlock()
		close(acquired)
		<-finish
		return nil
	})
	var releaseOnce, finishOnce sync.Once
	releaseHolder := func() { releaseOnce.Do(release) }
	finishWaiter := func() { finishOnce.Do(func() { close(finish) }) }
	defer func() { releaseHolder(); finishWaiter(); _ = action.wait(t) }()
	waitUploadCondition(t, func() bool { return uploadLockRefs(&registry, "upload") == 2 })
	if unlock := registry.tryLock(" upload "); unlock != nil {
		unlock()
		t.Fatal("try lock passed holder")
	}
	if refs := uploadLockRefs(&registry, "upload"); refs != 2 {
		t.Fatalf("failed try changed references: %d", refs)
	}
	releaseHolder()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter lost its lock")
	}
	if unlock := registry.tryLock("upload"); unlock != nil {
		unlock()
		t.Fatal("waiter got a different mutex")
	}
	finishWaiter()
	if err := action.wait(t); err != nil {
		t.Fatal(err)
	}
	if len(registry.entries) != 0 {
		t.Fatalf("registry leaked %d locks", len(registry.entries))
	}
}

func TestUploadLockRegistryConcurrentLifetime(t *testing.T) {
	var registry uploadLockRegistry
	var inside atomic.Int32
	var overlap atomic.Bool
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for n := 0; n < 64; n++ {
				key := "shared"
				if i%2 == 0 {
					key = " shared "
				}
				release := registry.lock(key)
				if inside.Add(1) != 1 {
					overlap.Store(true)
				}
				inside.Add(-1)
				release()
				// Unique IDs must also retire; the map must not grow with history.
				registry.lock(fmt.Sprintf("unique-%d-%d", i, n))()
			}
		}(i)
	}
	workers.Wait()
	if overlap.Load() {
		t.Fatal("two locks protected the same normalized ID")
	}
	if len(registry.entries) != 0 {
		t.Fatalf("registry leaked %d locks", len(registry.entries))
	}
}
