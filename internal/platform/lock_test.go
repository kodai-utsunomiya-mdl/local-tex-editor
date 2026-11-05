package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFolderLockBlocksAnotherGoroutineUntilReleased(t *testing.T) {
	for name, acquire := range map[string]func(string) (func(), error){
		"application": AcquireAppLock,
		"project":     AcquireProjectLock,
	} {
		t.Run(name, func(t *testing.T) {
			folder := t.TempDir()
			if err := os.Mkdir(filepath.Join(folder, ".local-tex-editor"), 0700); err != nil {
				t.Fatal(err)
			}
			release, err := acquire(folder)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			result := make(chan error, 1)
			go func() {
				otherRelease, otherErr := acquire(folder)
				if otherRelease != nil {
					otherRelease()
				}
				result <- otherErr
			}()
			select {
			case err := <-result:
				if !errors.Is(err, ErrLocked) {
					t.Fatalf("another goroutine acquired the same folder: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("acquiring an occupied lock blocked")
			}
			release()
			again, err := acquire(folder)
			if err != nil {
				t.Fatalf("could not acquire the folder after release: %v", err)
			}
			again()
		})
	}
}
