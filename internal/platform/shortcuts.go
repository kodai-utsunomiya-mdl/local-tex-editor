package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const shortcutApplication = "github.com/kodai-utsunomiya-mdl/local-tex-editor"
const shortcutExecutableName = "local-tex-editor.exe"
const shortcutManifestName = "installation.json"
const shortcutName = "Local TeX Editor.lnk"
const shortcutExecutableLimit int64 = 512 << 20

var ErrShortcutsUnsupported = errors.New("起動用のショートカット作成は Windows で利用できます．")
var ErrShortcutConflict = errors.New("作成先のファイルを確認してください．")

type ShortcutResult struct {
	Executable string `json:"executable"`
	Shortcut   string `json:"shortcut"`
	Message    string `json:"message"`
}

type shortcutInstallation struct {
	Application string `json:"application"`
	Format      int    `json:"format"`
	SHA256      string `json:"sha256"`
}

func shortcutPathsEqual(first, second string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(first), filepath.Clean(second))
	}
	return filepath.Clean(first) == filepath.Clean(second)
}

func shortcutInstallDirectory(localAppData string) (string, error) {
	if !filepath.IsAbs(localAppData) {
		return "", errors.New("アプリの保存先を取得できません．")
	}
	directory := localAppData
	for _, component := range []string{"Programs", "LocalTeXEditor"} {
		directory = filepath.Join(directory, component)
		if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("アプリの保存先を作成できません (%s): %w", directory, err)
		}
		info, err := os.Lstat(directory)
		if err != nil {
			return "", err
		}
		linked, err := IsReparsePoint(directory)
		if err != nil {
			return "", err
		}
		if linked || !info.IsDir() {
			return "", fmt.Errorf("アプリの保存先が通常のフォルダーではありません (%s)．%w", directory, ErrShortcutConflict)
		}
	}
	return directory, nil
}

func shortcutRegularFile(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	linked, err := IsReparsePoint(path)
	if err != nil {
		return nil, err
	}
	regular, err := IsRegularFile(path, info)
	if err != nil {
		return nil, err
	}
	if linked || !regular {
		return nil, fmt.Errorf("通常のファイルとして扱えません (%s)．%w", path, ErrShortcutConflict)
	}
	return info, nil
}

func shortcutFileHash(path string) (string, error) {
	info, err := shortcutRegularFile(path)
	if err != nil {
		return "", err
	}
	if info.Size() <= 0 || info.Size() > shortcutExecutableLimit {
		return "", fmt.Errorf("アプリの実行ファイルのサイズを確認してください (%s)．%w", path, ErrShortcutConflict)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, shortcutExecutableLimit+1))
	if err != nil {
		return "", err
	}
	if written != info.Size() {
		return "", errors.New("実行ファイルが読み込み中に変更されました．もう一度操作してください．")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readShortcutInstallation(path string) (*shortcutInstallation, error) {
	info, err := shortcutRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Size() > 4096 {
		return nil, fmt.Errorf("この保存先に別の installation.json があります (%s)．%w", path, ErrShortcutConflict)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return nil, err
	}
	var value shortcutInstallation
	if len(data) > 4096 || json.Unmarshal(data, &value) != nil || value.Application != shortcutApplication || value.Format != 1 {
		return nil, fmt.Errorf("この保存先に別の installation.json があります (%s)．%w", path, ErrShortcutConflict)
	}
	if hash, err := hex.DecodeString(value.SHA256); err != nil || len(hash) != sha256.Size {
		return nil, fmt.Errorf("アプリの配置情報を読めません (%s)．%w", path, ErrShortcutConflict)
	}
	return &value, nil
}

// The caller supplies its own executable and a directory obtained from the
// current user's known folders. API request fields never supply either path.
func installShortcutExecutable(source, directory string) (string, error) {
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("起動中の実行ファイルを開けません: %w", err)
	}
	sourceHash, err := shortcutFileHash(source)
	if err != nil {
		return "", err
	}
	target := filepath.Join(directory, shortcutExecutableName)
	manifestPath := filepath.Join(directory, shortcutManifestName)
	manifest, err := readShortcutInstallation(manifestPath)
	if err != nil {
		return "", err
	}
	existing, err := shortcutRegularFile(target)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	copyNeeded := !exists
	if exists {
		targetHash, err := shortcutFileHash(target)
		if err != nil {
			return "", err
		}
		copyNeeded = targetHash != sourceHash
		if copyNeeded && (manifest == nil || manifest.SHA256 != targetHash) {
			return "", fmt.Errorf("配置先の実行ファイルが，このアプリの配置記録と一致しません (%s)．%w", target, ErrShortcutConflict)
		}
		if shortcutPathsEqual(source, target) {
			copyNeeded = false
		}
	}
	if copyNeeded {
		staged, err := stageShortcutExecutable(source, directory, sourceHash)
		if err != nil {
			return "", err
		}
		defer os.Remove(staged)
		if exists {
			current, err := shortcutRegularFile(target)
			if err != nil || !os.SameFile(existing, current) || existing.Size() != current.Size() || !existing.ModTime().Equal(current.ModTime()) {
				return "", fmt.Errorf("配置先の実行ファイルが操作中に変更されました．%w", ErrShortcutConflict)
			}
		}
		if err := commitShortcutFile(staged, target, exists); err != nil {
			return "", fmt.Errorf("配置先のアプリを更新できません (%s)．起動中の Local TeX Editor を終了し，新しい実行ファイルを開いてから再度操作してください．%w (%v)", target, ErrShortcutConflict, err)
		}
	}
	if manifest == nil || manifest.SHA256 != sourceHash {
		data, err := json.MarshalIndent(shortcutInstallation{Application: shortcutApplication, Format: 1, SHA256: sourceHash}, "", "  ")
		if err != nil {
			return "", err
		}
		if err := writeShortcutInstallation(manifestPath, append(data, '\n'), manifest != nil); err != nil {
			return "", fmt.Errorf("アプリの配置情報を保存できません: %w", err)
		}
	}
	return target, nil
}

func stageShortcutExecutable(source, directory, expectedHash string) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.CreateTemp(directory, "local-tex-editor-*.tmp")
	if err != nil {
		return "", err
	}
	name := output.Name()
	done := false
	defer func() {
		_ = output.Close()
		if !done {
			_ = os.Remove(name)
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, shortcutExecutableLimit+1)); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != expectedHash {
		return "", errors.New("実行ファイルがコピー中に変更されました．もう一度操作してください．")
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	if err := output.Close(); err != nil {
		return "", err
	}
	done = true
	return name, nil
}

func writeShortcutInstallation(path string, data []byte, replace bool) error {
	file, err := os.CreateTemp(filepath.Dir(path), "installation-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return commitShortcutFile(file.Name(), path, replace)
}
