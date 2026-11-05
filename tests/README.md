# Windows での動作確認

`editor.spec.mjs` は，配布する実行ファイルを起動し，実際の TeX Live と Chromium を使用する．テスト用の原稿は `fixtures/japanese` にある．ユーザーの原稿を CI へ送る処理は含めていない．

次の操作を確認する．

- 起動時に誤ったアドレスを指定した場合，`--no-browser` では画面の操作を待たず，標準エラーと終了コードを返す．
- 日本語と空白を含むフォルダーを画面から開く．
- 日本語のコメントを編集して保存する．
- プロジェクトの `latexmkrc` を読み，upLaTeX，upBibTeX，dvipdfmx で本文と参考文献をコンパイルする．
- PDF の一部を HTTP Range で受け取り，ブラウザで描画する．
- 二ページ目を表示してから再コンパイルし，そのページが引き続き表示されることを確認する．
- 外部で原稿が変更された場合に，画面の編集内容とディスク上の変更が残り，競合が表示されることを確認する．
- UTF-8 BOM と CRLF を含むファイルを保存する．
- 認証がないリクエスト，異なる Origin，プロジェクト外のパスを拒否する．
- TeX のエラーと終了コードを表示し，最後に成功した PDF を表示する．
- 実行中の TeX を停止し，続けて通常のコンパイルを行う．

Windows のワークフローでは [r-lib/actions/setup-tinytex](https://github.com/r-lib/actions/tree/v2/setup-tinytex) を使い，CI のための TeX Live を用意する．日本語のパッケージは `tlmgr` で導入する．利用者の PC へ TeX Live を導入する操作はアプリに含まれない．

## 開発環境で実行する

フロントエンドをビルドした後，実行ファイルを `bin/local-tex-editor.exe` に作成する．`LOCAL_TEX_EDITOR_TEST_EXE` で別の場所も指定できる．TeX Live の実行ファイルがある場所は `LOCAL_TEX_EDITOR_TEST_TEX_BIN` で指定する．

```powershell
npm ci
npm run build
go test ./...
go build -o bin/local-tex-editor.exe ./cmd/local-tex-editor
$env:LOCAL_TEX_EDITOR_TEST_TEX_BIN = 'C:\texlive\2025\bin\windows'
npx playwright install chromium
npm test
```

テストは一時フォルダーへ原稿のコピーを作り，終了時に削除する．失敗時は `test-results` にブラウザの画像と trace，アプリの出力を保存する．成功したブラウザの画面も同じ場所へ保存する．GitHub Actions は，すべての確認が成功した後に，その確認で使った実行ファイルを `local-tex-editor-windows-x64` へ保存する．

日本語のコメントの入力は Unicode 文字列を使って確認する．Windows の各 IME の候補選択や変換中のキー操作は，実際の執筆画面でも確認する．
