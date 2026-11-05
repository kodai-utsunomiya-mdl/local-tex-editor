# Windows での動作確認

`editor.spec.mjs` は，配布する実行ファイルを起動し，実際の TeX Live と Chromium を使用する．テスト用の原稿は `fixtures/japanese` にある．ユーザーの原稿を CI へ送る処理は含めていない．

次の操作を確認する．

- 起動時に誤ったアドレスを指定した場合，`--no-browser` では画面の操作を待たず，標準エラーと終了コードを返す．
- 日本語と空白を含むフォルダーを画面から開く．
- 日本語のコメントを編集して保存する．
- 複数行の TeX をクリップボードから検索欄へ貼り付け，前後へ移動して置換する．TeX の `\` と改行が保存後も同じであることを確認する．
- 空フォルダー，PNG，SVG，PDF をファイル一覧から開き，編集中の原稿へ戻る．カーソルと草稿を確認する．
- 1440 × 900 の画面で，上部の操作欄と原稿・PDF の開始位置を確認する．
- プロジェクトの `latexmkrc` を読み，upLaTeX，upBibTeX，dvipdfmx で本文と参考文献をコンパイルする．
- PDF の一部を HTTP Range で受け取り，ブラウザで描画する．
- PDF をマウスホイールで続けて読み，倍率と画面のテーマを変更する．
- 設定の文字サイズ，エディタ上の Ctrl + ホイールと 2 点のタッチ操作で原稿を拡大・縮小する．原稿の bytes，PDF の幅・ページ・更新状態，ブラウザ全体の倍率が同じであることを確認する．
- PDF 内の日本語を検索し，そのページを表示する．
- 二ページ目を表示してから再コンパイルし，そのページが引き続き表示されることを確認する．
- 日本語と空白を含む `.tex` のファイル名を使い，SyncTeX で原稿の行から PDF へ移動し，PDF の文章からその原稿を開く．
- アプリを起動し直し，前回の PDF と SyncTeX の情報を読み戻す．
- 自動保存でディスクへ書き込み，アウトラインから節へ移動し，参考文献のキーを補完する．
- 自動保存を切った状態で草稿を保存し，アプリの再起動後にその草稿をエディタへ戻す．
- 150 ページの単純な原稿をコンパイルし，末尾へ移動する．ブラウザの canvas の数が 20 以下であることを確認し，その数と画素数を記録する．
- 外部で原稿が変更された場合に，画面の編集内容とディスク上の変更が残り，競合が表示されることを確認する．
- UTF-8 BOM と CRLF を含むファイルを保存する．
- 認証がないリクエスト，異なる Origin，プロジェクト外のパスを拒否する．
- TeX のエラーと終了コードを表示し，エラーの行へ移動する．最後に成功した PDF も表示する．
- 実行中の TeX を停止し，続けて通常のコンパイルを行う．

Windows のワークフローでは [r-lib/actions/setup-tinytex](https://github.com/r-lib/actions/tree/v2/setup-tinytex) を使い，CI のための TeX Live を用意する．日本語のパッケージは `tlmgr` で導入する．利用者の PC へ TeX Live を導入する操作はアプリに含まれない．

## 開発環境で実行する

フロントエンドをビルドした後，実行ファイルを `bin/local-tex-editor.exe` に作成する．`LOCAL_TEX_EDITOR_TEST_EXE` で別の場所も指定できる．TeX Live の実行ファイルがある場所は `LOCAL_TEX_EDITOR_TEST_TEX_BIN` で指定する．

```powershell
npm ci
npm run test:unit
npm run build
go test ./...
go build -o bin/local-tex-editor.exe ./cmd/local-tex-editor
$env:LOCAL_TEX_EDITOR_TEST_TEX_BIN = 'C:\texlive\2025\bin\windows'
npx playwright install chromium
npm test
```

テストは一時フォルダーへ原稿のコピーを作り，終了時に削除する．失敗時は `test-results` にブラウザの画像と trace，アプリの出力を保存する．成功したブラウザの画面も同じ場所へ保存する．GitHub Actions は，すべての確認が成功した後に，その確認で使った実行ファイルを `local-tex-editor-windows-x64` へ保存する．

日本語のコメントの入力は Unicode 文字列を使って確認する．Windows の各 IME の候補選択や変換中のキー操作は，実際の執筆画面でも確認する．
