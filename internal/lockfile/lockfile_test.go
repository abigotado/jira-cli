package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWithCreatesAndReleasesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "registry.json")
	lock := path + ".lock"
	called := false
	if err := With(path, func() error {
		called = true
		if _, err := os.Stat(lock); err != nil {
			t.Errorf("lock is not held during callback: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("With: %v", err)
	}
	if !called {
		t.Error("callback was not called")
	}
	if info, err := os.Stat(lock); err != nil || !info.Mode().IsRegular() {
		t.Errorf("persistent lock file is unavailable: %v", err)
	}
}

func TestWithReleasesLockAfterCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	want := errors.New("write failed")
	if err := With(path, func() error { return want }); !errors.Is(err, want) {
		t.Errorf("With error = %v, want callback error", err)
	}
	if info, err := os.Stat(path + ".lock"); err != nil || !info.Mode().IsRegular() {
		t.Errorf("persistent lock file is unavailable: %v", err)
	}
}

func TestWithSerializesConcurrentCallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := With(path, func() error {
			close(entered)
			<-release
			return nil
		}); err != nil {
			t.Errorf("first With: %v", err)
		}
	}()
	<-entered

	secondEntered := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := With(path, func() error {
			close(secondEntered)
			return nil
		}); err != nil {
			t.Errorf("second With: %v", err)
		}
	}()

	select {
	case <-secondEntered:
		t.Fatal("second callback ran while the first still held the lock")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second callback did not run after lock release")
	}
	wg.Wait()
}

func TestOldMtimeCannotStealLiveAdvisoryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- With(path, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	secondEntered := false
	file, err := os.OpenFile(path+".lock", os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	err = acquire(file, path+".lock", 20*time.Millisecond)
	var timeout *TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("acquire error = %v, want TimeoutError", err)
	}
	if err == nil {
		secondEntered = true
	}
	_ = file.Close()
	if secondEntered {
		t.Fatal("old mtime allowed a live lock to be stolen")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("holder: %v", err)
	}
}

func TestWithRejectsNilCallback(t *testing.T) {
	if err := With(filepath.Join(t.TempDir(), "x"), nil); err == nil {
		t.Error("nil callback was accepted")
	}
}
