package tasks

import (
	"testing"
	"time"
)

func TestWorkerHostActionLockCleansUp(t *testing.T) {
	w := NewWorker(nil, nil)

	unlock := w.lockHostAction("host-1")
	if got := len(w.hostLocks); got != 1 {
		t.Fatalf("expected one host lock, got %d", got)
	}

	unlock()
	if got := len(w.hostLocks); got != 0 {
		t.Fatalf("expected host lock cleanup, got %d", got)
	}
}

func TestWorkerHostActionLockKeepsWaiterOnSameLock(t *testing.T) {
	w := NewWorker(nil, nil)

	unlockFirst := w.lockHostAction("host-1")
	acquiredSecond := make(chan func(), 1)
	go func() {
		acquiredSecond <- w.lockHostAction("host-1")
	}()

	deadline := time.Now().Add(time.Second)
	for {
		w.lockMu.Lock()
		lock := w.hostLocks["host-1"]
		refs := 0
		if lock != nil {
			refs = lock.refs
		}
		w.lockMu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for second host lock waiter")
		}
		time.Sleep(time.Millisecond)
	}

	unlockFirst()
	unlockSecond := <-acquiredSecond
	if got := len(w.hostLocks); got != 1 {
		t.Fatalf("expected host lock to remain while second holder is active, got %d", got)
	}

	unlockSecond()
	if got := len(w.hostLocks); got != 0 {
		t.Fatalf("expected host lock cleanup after second holder unlocks, got %d", got)
	}
}
