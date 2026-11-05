package main

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestWorkerMIMEDoesNotUseWindowsFileAssociations(t *testing.T) {
	previous := mime.TypeByExtension(".mjs")
	if err := mime.AddExtensionType(".mjs", "text/plain"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mime.AddExtensionType(".mjs", previous) })
	module := "export const loaded = true;\n"
	assets := fstest.MapFS{"pdf.worker.min.mjs": {Data: []byte(module)}}
	handler := staticFiles("127.0.0.1:8765", assets)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/pdf.worker.min.mjs", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != module {
		t.Fatalf("worker response: status %d, body %q", response.Code, response.Body.String())
	}
	mediaType, _, err := mime.ParseMediaType(response.Header().Get("Content-Type"))
	if err != nil || mediaType != "text/javascript" {
		t.Fatalf("a browser cannot load this module with Content-Type %q", response.Header().Get("Content-Type"))
	}
}
