package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitForPicker(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(description)
}

func pickerAsync(picker *folderPicker, ctx context.Context) <-chan folderPickResult {
	result := make(chan folderPickResult, 1)
	go func() {
		path, err := picker.pick(ctx)
		result <- folderPickResult{path, err}
	}()
	return result
}

func pickerResult(t *testing.T, result <-chan folderPickResult) folderPickResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("the folder request did not finish")
		return folderPickResult{}
	}
}

func TestFolderPickerSharesTheDialogAndFocusRequest(t *testing.T) {
	started := make(chan *folderPickRequest, 2)
	selection := make(chan string, 1)
	picker := &folderPicker{show: func(request *folderPickRequest) (string, error) {
		started <- request
		select {
		case path := <-selection:
			return path, nil
		case <-request.ctx.Done():
			return "", request.ctx.Err()
		}
	}}
	t.Cleanup(picker.close)
	firstContext, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := pickerAsync(picker, firstContext)
	request := <-started
	<-request.focus
	second := pickerAsync(picker, context.Background())
	waitForPicker(t, "two requests did not share the dialog", func() bool {
		picker.mu.Lock()
		defer picker.mu.Unlock()
		return request.waiters == 2
	})
	select {
	case <-request.focus:
	case <-time.After(time.Second):
		t.Fatal("the second request did not bring the existing dialog forward")
	}
	cancelFirst()
	if result := pickerResult(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled request = %+v", result)
	}
	if request.ctx.Err() != nil {
		t.Fatal("cancelling one of two callers closed the shared dialog")
	}
	selection <- "a selected folder"
	if result := pickerResult(t, second); result.path != "a selected folder" || result.err != nil {
		t.Fatalf("selected folder = %+v", result)
	}
	if len(started) != 0 {
		t.Fatal("a second native dialog opened")
	}
}

func TestFolderPickerWaitsForCancellationBeforeReopening(t *testing.T) {
	started := make(chan *folderPickRequest, 2)
	allowClose := make(chan struct{})
	picker := &folderPicker{show: func(request *folderPickRequest) (string, error) {
		started <- request
		<-request.ctx.Done()
		<-allowClose
		return "", nil
	}}
	t.Cleanup(picker.close)
	firstContext, cancelFirst := context.WithCancel(context.Background())
	first := pickerAsync(picker, firstContext)
	request := <-started
	cancelFirst()
	if result := pickerResult(t, first); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("cancelled request = %+v", result)
	}
	if request.ctx.Err() == nil {
		t.Fatal("the native dialog was not told to close")
	}
	second := pickerAsync(picker, context.Background())
	select {
	case <-started:
		t.Fatal("another dialog opened before the first one closed")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowClose)
	select {
	case next := <-started:
		if next == request || next.ctx.Err() != nil {
			t.Fatal("the next selection reused a cancelled native request")
		}
	case <-time.After(time.Second):
		t.Fatal("a new selection could not open after cancellation")
	}
	picker.close()
	if result := pickerResult(t, second); result.err != nil || result.path != "" {
		t.Fatalf("closing the dialog = %+v", result)
	}
}
