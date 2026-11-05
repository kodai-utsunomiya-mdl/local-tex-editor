package app

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func findSynctex(bin string) (string, error) {
	if bin == "" {
		return "", fail(http.StatusServiceUnavailable, "TeX Live の実行ファイルが入ったフォルダーを設定してください．")
	}
	name := "synctex"
	if runtime.GOOS == "windows" {
		// TeX Live's synctex.windows package supplies this native executable.
		name += ".exe"
	}
	path := filepath.Join(bin, name)
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return filepath.Abs(path)
	}
	return "", fail(http.StatusServiceUnavailable, "%s が見つかりません．設定画面の SyncTeX の案内を確認してください．", path)
}

func synctexStatus(bin string) (string, string, []string) {
	tool, err := findSynctex(bin)
	if err == nil {
		return tool, "", []string{}
	}
	commands := []string{}
	if runtime.GOOS == "windows" && bin != "" {
		manager := filepath.Join(bin, "tlmgr.bat")
		if info, statErr := os.Stat(manager); statErr == nil && info.Mode().IsRegular() {
			commands = synctexRepairCommands(bin, texLiveYear(bin), time.Now().Year())
		}
	}
	return "", err.Error(), commands
}

func texLiveYear(bin string) int {
	root := filepath.Dir(filepath.Dir(bin))
	if file, err := os.Open(filepath.Join(root, "release-texlive.txt")); err == nil {
		text, _ := io.ReadAll(io.LimitReader(file, 4096))
		_ = file.Close()
		for _, line := range strings.Split(string(text), "\n") {
			if strings.Contains(line, "TeX Live") {
				for _, field := range strings.Fields(line) {
					if year := parseTeXLiveYear(field); year != 0 {
						return year
					}
				}
			}
		}
	}
	// This record is near the beginning of texlive.tlpdb. Reading a bounded
	// prefix avoids loading the package database on every settings refresh.
	if file, err := os.Open(filepath.Join(root, "tlpkg", "texlive.tlpdb")); err == nil {
		scanner := bufio.NewScanner(io.LimitReader(file, 1<<20))
		inConfig := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "name ") {
				if inConfig {
					break
				}
				inConfig = line == "name 00texlive.config"
			}
			if inConfig && strings.HasPrefix(line, "depend release/") {
				year := parseTeXLiveYear(strings.TrimPrefix(line, "depend release/"))
				_ = file.Close()
				if year != 0 {
					return year
				}
				break
			}
		}
		_ = file.Close()
	}
	return parseTeXLiveYear(filepath.Base(root))
}

func parseTeXLiveYear(text string) int {
	if len(text) != 4 {
		return 0
	}
	year, err := strconv.Atoi(text)
	if err != nil || year < 2008 || year > 2100 {
		return 0
	}
	return year
}

func powershellLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

func synctexRepairCommands(bin string, year, currentYear int) []string {
	if year == 0 || year > currentYear {
		return []string{}
	}
	repository := "https://mirror.ctan.org/systems/texlive/tlnet"
	if year < currentYear {
		repository = fmt.Sprintf("https://ftp.math.utah.edu/pub/tex/historic/systems/texlive/%d/tlnet-final", year)
	}
	return []string{
		"& " + powershellLiteral(filepath.Join(bin, "tlmgr.bat")) + " --repository " + powershellLiteral(repository) + " install --reinstall synctex synctex.windows",
		"& " + powershellLiteral(filepath.Join(bin, "synctex.exe")) + " help",
	}
}
