# Local TeX Editor

Windows のブラウザで TeX の原稿を編集し，PC にある TeX Live でコンパイルするアプリ．編集画面と PDF を同じウィンドウで確認できる．アプリを起動すると，既定のブラウザで `localhost` の画面が開く．

原稿は PC 上のフォルダーに保存する．TeX Live と原稿の `latexmkrc` を使い，コンパイル中のログと生成した PDF を画面へ表示する．利用する PC に追加の Python，Node.js，Go，VS Code を導入する必要はない．

## ダウンロードして起動する

1. [Releases](https://github.com/kodai-utsunomiya-mdl/local-tex-editor/releases) を開き，最新のバージョンを選ぶ．
2. **Assets** の `local-tex-editor-0.1.0-windows-x64.zip` をダウンロードする．`Source code` は開発用のソースである．
3. ZIP を展開し，`local-tex-editor.exe` をダブルクリックする．
4. 開いたブラウザで原稿のフォルダーを選ぶ．フォルダーのパスを入力して開くこともできる．
5. TeX Live の場所が自動で見つからない場合は，設定に `C:\texlive\2025\bin\windows` など，`latexmk.exe` があるフォルダーを指定する．
6. メイン文書を `main.tex` にして，コンパイルする．

Overleaf にある最新の原稿は，プロジェクトのソースを ZIP としてダウンロードして展開し，そのフォルダーを開く．GitHub から取得した原稿を開く場合も，`.tex`，`.bib`，画像，`latexmkrc` を含むプロジェクトのフォルダーを指定する．

アプリを終了する際は，画面の終了ボタンを使う．コンパイル中の処理も終了する．

起動済みの状態で `.exe` をもう一度開くと，既存の編集画面へ接続する．同じ原稿を複数のプロセスが同時にコンパイルすることを防ぐ仕組みも入れている．

## 原稿の編集

- 左側のファイル一覧から `.tex`，`.bib` などを開く．
- `Ctrl + S` で保存する．コンパイル操作でも，編集中のファイルを保存してから TeX を実行する．
- `Ctrl + Enter` でコンパイルする．
- エディタの検索機能で文字列を検索・置換する．
- 別のアプリが同じファイルを書き換えた場合は，保存の競合を表示する．
- PDF はページを指定して読み進め，倍率を変更できる．更新後も同じページ付近を表示する．
- コンパイルの結果とログは画面で確認できる．最新のコンパイルが失敗した場合も，前回成功した PDF を参照できる．

## コンパイルの設定

既存の `latexmkrc` がある原稿では，設定画面のコンパイラーを「latexmkrc の設定を使う」にする．`latexmkrc` / `.latexmkrc` に書かれた `uplatex`，`upbibtex`，`dvipdfmx` などの指定を使う．

`latexmkrc` を置かないプロジェクトでは，pdfLaTeX，upLaTeX，XeLaTeX，LuaLaTeX を選択できる．アプリは `latexmk` を実行し，参考文献や相互参照に必要な再実行も `latexmk` が担当する．

コンパイルの中間ファイルは，プロジェクト内の `.local-tex-editor/build` に保存する．次回のコンパイルでは，同じ中間ファイルを使う．プロジェクトを Git で管理する場合は，そのプロジェクトの `.gitignore` に次を追加できる．

```gitignore
.local-tex-editor/
```

コンパイル時間の上限はアプリ側で設けていない．実行中のコンパイルは，画面の停止ボタンから停止できる．

## アプリとデータの保存先

編集画面に必要な JavaScript，PDF.js の worker，フォントなどは実行ファイルに含む．起動のたびに CDN から取得する処理はない．

設定と表示用の PDF のコピーは，Windows ユーザーの設定フォルダーに保存する．原稿そのものは，選択したプロジェクトのフォルダーへ保存する．PDF のコピーを使うことで，TeX が書き込み中の PDF と表示中の PDF が競合する状況を防ぐ．

接続は `127.0.0.1` などの loopback のアドレスに限定する．起動ごとに生成する token を使い，ファイルの読み書きとコンパイルの API を呼び出す．

## 開発する

以下はアプリを開発する環境での手順である．配布済みの `.exe` を使う PC では実行しない．

必要なものは Node.js 22.13 以降と Go 1.25 以降である．Windows での動作確認には TeX Live / TinyTeX と Chromium も使う．

```sh
npm ci
npm run build
go test ./...
go build -o bin/local-tex-editor.exe ./cmd/local-tex-editor
```

Windows 用の配布ファイルは次のコマンドで作成する．

```sh
go build -trimpath -ldflags="-s -w -H=windowsgui -X main.version=0.1.0" -o dist/local-tex-editor.exe ./cmd/local-tex-editor
```

起動時のオプション:

```text
--project PATH       開くプロジェクトのフォルダー
--tex-bin PATH       TeX Live の実行ファイルのフォルダー
--no-browser         ブラウザを自動で開かない
--listen ADDRESS     loopback のアドレスとポート (既定: 127.0.0.1:0)
--config-dir PATH    設定とキャッシュの保存先
--ready-file PATH    起動したサーバーの URL と token を書くファイル
--version            アプリのバージョンを表示する
```

`--ready-file` は自動テストなどで使用する．このファイルには，アプリを操作するための token が含まれる．アプリを終了すると削除する．

GitHub Actions は Windows で Go のテストとブラウザのテストを実行し，成功した実行のアプリを artifact として保存する．短い日本語の原稿と参考文献を使い，保存，コンパイル，PDF の表示，エラーの表示，停止を確認する．

`main` のテストが成功し，`package.json` のバージョンに対応する Release がまだ存在しない場合は，その実行で確認したアプリを Releases に公開する．新しいバージョンを作る際は，`package.json` を更新し，`release-notes/v<バージョン>.md` を追加する．各ブランチで確認したアプリは，Actions の **Artifacts** にある `local-tex-editor-windows-x64` からも取得できる．

## 利用するライブラリ

- [CodeMirror](https://codemirror.net/): 編集画面 (MIT)
- [PDF.js](https://mozilla.github.io/pdf.js/): PDF の表示 (Apache-2.0)
- [Go](https://go.dev/): HTTP サーバーと TeX の実行

依存する JavaScript のバージョンは `package-lock.json` に記録する．同梱するライブラリのライセンス本文は，アプリの `/THIRD_PARTY_LICENSES.txt` に含む．アプリ自体のライセンスは [MIT](LICENSE) である．
