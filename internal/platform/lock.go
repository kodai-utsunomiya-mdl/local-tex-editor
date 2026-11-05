package platform

import "errors"

// ErrLocked means that another process is already using the same folder.
var ErrLocked = errors.New("another process is already using this folder")

// The release function returned by AcquireAppLock and AcquireProjectLock must
// run in the goroutine that acquired the lock. Windows mutexes are owned by an
// OS thread, and that goroutine stays on its thread until it releases the lock.
