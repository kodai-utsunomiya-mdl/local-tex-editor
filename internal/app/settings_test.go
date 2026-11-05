package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestSettingsOnlyInvalidatePDFWhenCompilationSettingsChange(t *testing.T) {
	for _, test := range []struct {
		name           string
		change         string
		alreadyChanged bool
		wantChanged    bool
	}{
		{name: "no compiler settings"},
		{name: "same compiler settings", change: "same"},
		{name: "equivalent compiler path", change: "equivalent"},
		{name: "another main document", change: "main", wantChanged: true},
		{name: "another compilation mode", change: "mode", wantChanged: true},
		{name: "another TeX installation", change: "texBin", wantChanged: true},
		{name: "existing source edits remain", change: "same", alreadyChanged: true, wantChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, root := testApp(t)
			texBin := filepath.Join(t.TempDir(), "TeX Live", "bin")
			a.config.TexBin = texBin
			pdfPath := filepath.Join(t.TempDir(), "compiled.pdf")
			writeTestFile(t, pdfPath, []byte("%PDF-1.7\nprevious successful build\n"))
			a.lastPDF, a.lastPDFID = pdfPath, "existing-build"
			a.build = &buildRun{result: Build{ID: "existing-build", Status: "success", SourceChanged: test.alreadyChanged}, started: time.Now(), finished: time.Now()}
			body := map[string]string{}
			if test.change != "" {
				body = map[string]string{"texBin": texBin, "main": a.project.Main, "mode": a.project.Mode}
			}
			switch test.change {
			case "equivalent":
				body["texBin"] = "  " + texBin + string(filepath.Separator) + "  "
			case "main":
				writeTestFile(t, filepath.Join(root, "another.tex"), []byte("\\documentclass{article}\n\\begin{document}Another\\end{document}\n"))
				body["main"] = "another.tex"
			case "mode":
				body["mode"] = "uplatex"
			case "texBin":
				body["texBin"] = filepath.Join(t.TempDir(), "another TeX Live")
			}
			response := apiRequest(a, http.MethodPut, "/api/settings", body, map[string]string{"X-Project-Root": url.PathEscape(root)})
			var state State
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &state) != nil || state.Build == nil {
				t.Fatalf("settings: %d %s", response.Code, response.Body.String())
			}
			if state.Build.SourceChanged != test.wantChanged {
				t.Fatalf("sourceChanged = %v, want %v", state.Build.SourceChanged, test.wantChanged)
			}
			if state.Build.PDFBuildID != "existing-build" || !state.Build.PDFReady || state.Build.Status != "success" {
				t.Fatalf("settings replaced the previous result: %#v", state.Build)
			}
		})
	}
}
