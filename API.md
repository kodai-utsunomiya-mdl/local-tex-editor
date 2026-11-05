# Local TeX Editor の実装

Windows のブラウザでローカルの TeX プロジェクトを編集する．Go の HTTP サーバーが UI，API，PDF を同じ loopback のポートから配信する．原稿は選択したフォルダーに保存し，既存の latexmkrc を用いる．UI は CodeMirror 6 と PDF.js を使用し，ビルド時に Go の実行ファイルへ組み込む．Python，Node.js，Go は利用する PC に導入しない．

## 共通の約束

- API は JSON．失敗は適切な HTTP status と `{ "error": "説明" }`．画面は HTTP の失敗とプロセスの失敗を表示する．
- API と PDF は `X-Local-Token` ヘッダーで起動時のランダムな token を受け取る．UI は起動 URL の `#token=...` を読み，sessionStorage に置き，URL の fragment を取り除く．Host と Origin も検査する．サーバーは loopback だけに bind する．
- プロジェクトの操作には `X-Project-Root: encodeURIComponent(root)` を付ける．別のタブでプロジェクトが変わった場合は HTTP 409 を返す．
- パスはプロジェクトからの相対パス．絶対パス，親への移動，symlink / reparse point によるプロジェクト外へのアクセスを拒否する．プロジェクトを開く操作ではユーザーが選んだ絶対パスを受け付ける．
- ファイルは UTF-8．既存の CRLF と UTF-8 BOM を保存時に尊重する．読めない文字コードは画面へ説明する．ファイルの SHA-256 を revision として返し，保存時の競合を HTTP 409 で通知する．
- HTML / CSS / JS の配信には認証不要．ユーザーファイルやプロジェクトの情報は API の認証後に扱う．外部 CDN を利用しない．

## API

### GET /api/state

```json
{"version":"0.1.0","platform":"windows","texBin":"C:\\texlive\\2025\\bin\\windows","latexmk":"...","project":null,"recentProjects":[],"build":null}
```

project は開いている場合 `{ "root": "絶対パス", "main": "main.tex", "mode": "project", "files": [{"path":"main.tex","size":123}] }`．mode は `project`，`pdflatex`，`uplatex`，`xelatex`，`lualatex`．project では既存の latexmkrc をそのまま読み，出力形式を上書きしない．

### POST /api/project/open

入力 `{ "path": "絶対パス" }`．state と同じ形式を返す．コンパイル中は 409．

### POST /api/project/pick

Windows のフォルダー選択ダイアログを開く．成功時 `{ "path": "絶対パス" }`，取消時 `{ "path": "" }`．その他の OS では説明を返す．UI はパスの入力でも開ける．

### PUT /api/settings

入力 `{ "texBin": "絶対パスまたは空", "main": "main.tex", "mode": "project" }`．state を返す．texBin は TeX の実行ファイルがあるフォルダー．main / mode は開いた project 用．未指定の値は変更しない．

### GET /api/file?path=相対パス

`{"path":"main.tex","content":"...","revision":"sha256","lineEnding":"lf"}`．読み書きはテキストの拡張子を許可リストで限定する．

### PUT /api/file

入力 `{ "path":"main.tex", "content":"...", "revision":"読込時のsha256" }`．保存後に GET と同じ形式を返す．新規作成は revision を空文字列にし，既存なら 409．成功するまで UI の未保存状態を解除しない．同じフォルダーの一時ファイルへ書き込んで置き換える．

### POST /api/build

入力 `{}`．保存済みの main を latexmk でコンパイルし，即座に build を返す．同時実行は 409．ファイル変更はコンパイル前に UI から保存する．

### GET /api/build

```json
{"id":"...","status":"running","command":"...","log":"標準出力と標準エラー","exitCode":null,"error":"","startedAt":"ISO8601","finishedAt":"","pdfReady":false,"pdfBuildId":"","elapsedMs":0}
```

build がまだない場合は `null`．status は `running`，`success`，`failed`，`cancelled`．ログは上限を設けるが，エラーの理由と終了コードは必ず返す．最後に成功した PDF がある場合は失敗後も pdfReady=true とし，UI に最新のコンパイルが失敗した旨を表示する．PDF の版は pdfBuildId で識別する．

コンパイル開始後にアプリから保存が行われた場合は，`sourceChanged: true` を返す．画面は原稿が更新された旨を示す．

### POST /api/build/stop

現在のプロセスとその子を停止し，build を返す．

### GET /api/pdf

最後に成功した PDF のコピーを配信する．認証が必要．HTTP Range をサポートする．`?build=pdfBuildId` を指定すると，その版の同じ PDF を配信する．削除済みの版は HTTP 410 を返す．ファイルをコンパイル中に読む競合を避けるため，成功した PDF をアプリのキャッシュへコピーする．キャッシュはプロジェクトごとに現行版と直前の版までとする．

### POST /api/shutdown

コンパイルを停止し，`{"ok":true}` を返してサーバーを終了する．UI は未保存の編集がある場合に確認する．

## CLI と動作確認

- `--project PATH`，`--tex-bin PATH`，`--listen 127.0.0.1:0`，`--no-browser`，`--ready-file PATH`，`--config-dir PATH`．
- ready-file へ `{ "url": "http://127.0.0.1:port/", "token": "...", "pid": 123 }` を書く．同じ JSON を標準出力にも出す．
- 設定は OS のユーザー用設定フォルダーへ保存する．--config-dir はテスト用にも利用する．
- 同じ設定フォルダーで起動済みの場合，通常の起動は既存の画面を開く．コンパイルにはプロジェクトごとのロックを使い，異なる設定フォルダーからの同時起動でも生成物の競合を防ぐ．
- 起動時に main.tex を優先して候補を選ぶが，UI から選択を変更できる．
- Compile はプロジェクトのルートを working directory にして latexmk を実行する．既存の latexmkrc / .latexmkrc を使う．出力先は `-outdir=.local-tex-editor/build` とする．CLI に jobname の強制指定をしない．出力は main に対応する fls / fdb_latexmk と実際の PDF から見つけ，latexmk が成功したことを確かめる．
- 根拠のない No PDF を避けるため，実行の失敗，終了コード，PDF 探索結果を必ず build.error か build.log へ記す．
- 手書きの HTTP パーサーは使わず，Go 標準ライブラリを使う．

## 分担

- backend: internal/app 以下．アプリ状態，ファイル，設定，コンパイル，API．
- frontend: web/src 以下．CodeMirror，PDF.js，日本語の UI．
- tests: .github/workflows と tests 以下．Windows での TeX，ブラウザ，配布物の確認．
- root: cmd，internal/platform，web/embed.go，package.json，scripts，README，GitHub への反映．
