package platform

import (
	"context"
	"sync"
)

type folderPickResult struct {
	path string
	err  error
}

type folderPickRequest struct {
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	focus    chan struct{}
	waiters  int
	closing  bool
	finished bool
	result   folderPickResult
}

type folderPicker struct {
	mu     sync.Mutex
	active *folderPickRequest
	show   func(*folderPickRequest) (string, error)
}

var projectFolderPicker = folderPicker{show: showNativeFolderDialog}

// PickFolder uses one native dialog for concurrent requests. A second request
// brings that dialog forward, and cancelling the final request closes it.
func PickFolder(ctx context.Context) (string, error) {
	return projectFolderPicker.pick(ctx)
}

func CancelFolderPicker() {
	projectFolderPicker.close()
}

func (picker *folderPicker) pick(ctx context.Context) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		picker.mu.Lock()
		request := picker.active
		if request != nil && request.closing {
			picker.mu.Unlock()
			select {
			case <-request.done:
				continue
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if request == nil {
			nativeContext, cancel := context.WithCancel(context.Background())
			request = &folderPickRequest{ctx: nativeContext, cancel: cancel, done: make(chan struct{}), focus: make(chan struct{}, 1)}
			picker.active = request
			go picker.run(request)
		}
		request.waiters++
		picker.mu.Unlock()
		select {
		case request.focus <- struct{}{}:
		default:
		}
		select {
		case <-request.done:
			picker.release(request)
			return request.result.path, request.result.err
		case <-ctx.Done():
			picker.release(request)
			return "", ctx.Err()
		}
	}
}

func (picker *folderPicker) run(request *folderPickRequest) {
	path, err := picker.show(request)
	picker.mu.Lock()
	request.result = folderPickResult{path: path, err: err}
	request.finished = true
	if picker.active == request {
		picker.active = nil
	}
	close(request.done)
	picker.mu.Unlock()
	request.cancel()
}

func (picker *folderPicker) release(request *folderPickRequest) {
	picker.mu.Lock()
	defer picker.mu.Unlock()
	request.waiters--
	if request.waiters == 0 && !request.finished {
		request.closing = true
		request.cancel()
	}
}

func (picker *folderPicker) close() {
	picker.mu.Lock()
	defer picker.mu.Unlock()
	if request := picker.active; request != nil && !request.finished {
		request.closing = true
		request.cancel()
	}
}
