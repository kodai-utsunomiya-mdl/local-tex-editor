# Local TeX Editor の実装

Windows のブラウザでローカルの TeX プロジェクトを編集する．Go の HTTP サーバーが UI，API，PDF を同じ loopback のポートから配信する．原稿は選択したフォルダーに保存し，既存の latexmkrc を用いる．UI は CodeMirror 6 と PDF.js を使用し，ビルド時に Go の実行ファイルへ組み込む．Python，Node.js，Go は利用する PC に導入しない．

## 共通の約束

- API は JSON．PDF と画像のプレビューは各形式の bytes を返し，`/api/session` は接続を続けるための stream を返す．失敗は適切な HTTP status と `{ "error": "説明" }`．画面は HTTP の失敗とプロセスの失敗を表示する．
- API と PDF は `X-Local-Token` ヘッダーで起動時のランダムな token を受け取る．UI は起動 URL の `#token=...` を読み，sessionStorage に置き，URL の fragment を取り除く．Host と Origin も検査する．サーバーは loopback だけに bind する．
- プロジェクトの操作には `X-Project-Root: encodeURIComponent(root)` を付ける．別のタブでプロジェクトが変わった場合は HTTP 409 を返す．
- パスはプロジェクトからの相対パス．絶対パス，親への移動，symlink / reparse point によるプロジェクト外へのアクセスを拒否する．プロジェクトを開く操作ではユーザーが選んだ絶対パスを受け付ける．
- テキストの編集には UTF-8 を使う．既存の CRLF と UTF-8 BOM を保存時に尊重する．読めない文字コードは画面へ説明する．ファイルの SHA-256 を revision として返し，保存時の競合を HTTP 409 で通知する．
- HTML / CSS / JS の配信には認証不要．ユーザーファイルやプロジェクトの情報は API の認証後に扱う．外部 CDN を利用しない．

## API

### GET /api/state

```json
{"version":"0.1.3","executablePath":"C:\\Users\\user\\AppData\\Local\\Programs\\LocalTeXEditor\\local-tex-editor.exe","platform":"windows","shortcutsSupported":true,"texBin":"C:\\texlive\\2025\\bin\\windows","latexmk":"...","synctex":"...","synctexInstallCommands":[],"project":null,"recentProjects":[],"build":null}
```

`executablePath` は動いている実行ファイルの絶対パス．配置済みのアプリやショートカットの更新が失敗した場合は，`installationError` も返す．画面にはアプリのバージョンとパス，更新に失敗した際の案内を表示する．

synctex は利用できる実行ファイルのパス．見つからない場合は空文字列と synctexError の説明を返す．Windows で TeX Live の管理ツールと年版が確認できた場合は，設定画面に表示する PowerShell のコマンドを synctexInstallCommands へ入れる．

project は開いている場合に次の形式で返す．

```json
{"root":"絶対パス","main":"main.tex","mode":"project","files":[{"path":"main.tex","size":123,"kind":"text"},{"path":"figures/chart.png","size":456,"kind":"image"}],"directories":["figures","figures/empty"]}
```

mode は `project`，`pdflatex`，`uplatex`，`xelatex`，`lualatex`．project では既存の latexmkrc をそのまま読み，出力形式を上書きしない．

files は通常のファイルを一覧にし，path は `/` を使うルートからの相対パス，size は byte 数とする．kind は `text`，`image`，`pdf`，`binary`．UI は `text` を編集・草稿・TeX の補完の読込対象にする．`image` / `pdf` はプレビュー，`binary` は名前・サイズと未対応の種類である旨を表示する．

directories はルートからの相対パスの配列で，空のフォルダーも含む．ルート自体は含めない．`.git`，`.local-tex-editor`，`node_modules` 等の作業用フォルダーと，symlink / junction を経由した項目は一覧から除く．Windows の OneDrive cloud placeholder は，cloud tag と通常ファイル / フォルダーの属性を確認して扱う．ファイルの読込時には Windows が必要な内容を取得する．

### POST /api/project/open

入力 `{ "path": "絶対パス" }`．state と同じ形式を返す．コンパイル中は 409．

### POST /api/project/pick

Windows のフォルダー選択ダイアログを開く．成功時 `{ "path": "絶対パス" }`，取消時 `{ "path": "" }`．その他の OS では説明を返す．UI はパスの入力でも開ける．

### PUT /api/settings

入力 `{ "texBin": "絶対パスまたは空", "main": "main.tex", "mode": "project" }`．state を返す．texBin は TeX の実行ファイルがあるフォルダー．main / mode は開いた project 用．未指定の値は変更しない．

main / mode / texBin が変わった場合は，成功済みの PDF がある build の `sourceChanged` を true にする．同じ設定の再送と空の入力では，そのフラグを現在の値のままにする．文字サイズ等の UI 設定は `/api/storage` へ保存する．

### GET /api/file?path=相対パス

`{"path":"main.tex","content":"...","revision":"sha256","lineEnding":"lf"}`．読み書きはテキストの拡張子を許可リストで限定する．

### PUT /api/file

入力 `{ "path":"main.tex", "content":"...", "revision":"読込時のsha256" }`．保存後に GET と同じ形式を返す．新規作成は revision を空文字列にし，既存なら 409．成功するまで UI の未保存状態を解除しない．同じフォルダーの一時ファイルへ書き込んで置き換える．

### GET /api/asset?path=相対パス

プロジェクト内の画像または PDF を返す．`X-Local-Token` と `X-Project-Root` は他のプロジェクト用 API と共通．HEAD と HTTP Range にも対応する．ファイル全体をサーバーのメモリへ読み込まず，128 MiB までのファイルを配信する．上限を超えた場合は 413．

対応する拡張子は PNG / APNG，JPEG，GIF，WebP，BMP，SVG，ICO，AVIF，PDF．拡張子に対応する MIME type を明示し，先頭データも確認する．SVG は SVG namespace のルート要素を持つ XML を受け付ける．未対応の種類，HTML，形式と拡張子が合わないデータは 415．テキスト用の `/api/file` では画像や PDF の読み書きを受け付けない．

画像は認証付き fetch で読み込んだ Blob を `<img>` へ渡し，PDF は PDF.js へ渡す．SVG を DOM へ挿入したり，`iframe` / `object` / `embed` で表示したりしない．[SVG 2 の image としての処理](https://www.w3.org/TR/SVG/latest/conform.html)では，script，操作，外部ファイルの参照が無効になる．API の応答にも `Content-Security-Policy: sandbox; default-src 'none'; ...` と `X-Content-Type-Options: nosniff` を付ける．

応答は `Cache-Control: no-store` とし，ファイルが同じ秒内に更新された場合も現在の bytes を返す．`Last-Modified` による 304 応答は行わない．パスの移動，プロジェクト外，symlink / junction 経由，通常ファイル以外の項目は拒否する．

### POST /api/build

入力 `{}`．保存済みの main を latexmk でコンパイルし，即座に build を返す．同時実行は 409．ファイル変更はコンパイル前に UI から保存する．

### GET /api/build

```json
{"id":"...","status":"running","command":"...","log":"標準出力と標準エラー","exitCode":null,"error":"","startedAt":"ISO8601","finishedAt":"","pdfReady":false,"pdfBuildId":"","synctexReady":false,"elapsedMs":0,"sourceChanged":false,"restored":false}
```

build がまだない場合は `null`．status は `running`，`success`，`failed`，`cancelled`．ログは上限を設けるが，エラーの理由と終了コードは必ず返す．最後に成功した PDF がある場合は失敗後も pdfReady=true とし，UI に最新のコンパイルが失敗した旨を表示する．PDF の版は pdfBuildId で識別する．

コンパイル開始後にアプリから保存が行われた場合は，`sourceChanged: true` を返す．画面は原稿が更新された旨を示す．

`synctexReady` は `pdfBuildId` の PDF に対応する SyncTeX データがある場合に true．プロジェクトを開いた際に前回の PDF を読み込んだ場合は `restored: true`．その時点の原稿と保存された SHA-256 を比べ，変更があれば `sourceChanged: true` とする．

### POST /api/build/stop

現在のプロセスとその子を停止し，build を返す．

### GET /api/pdf

最後に成功した PDF のコピーを配信する．認証が必要．HTTP Range をサポートする．`?build=pdfBuildId` を指定すると，その版の同じ PDF を配信する．削除済みの版は HTTP 410 を返す．ファイルをコンパイル中に読む競合を避けるため，成功した PDF をアプリのキャッシュへコピーする．キャッシュはプロジェクトごとに現行版と直前の版までとする．

PDF，対応する SyncTeX データ，原稿のハッシュと版の情報を設定フォルダーへ保存する．次の起動でも，同じプロジェクトの main / mode に対応する前回の PDF を再コンパイルせずに表示できる．キャッシュの不備がある場合もプロジェクトを開ける．

### POST /api/synctex/forward

入力 `{ "buildId":"表示中の PDF の版", "path":"章/原稿.tex", "line":42, "column":1 }`．path はプロジェクトからの相対パス．line / column は 1 始まりで，列が不明の場合は column を 0 とする．

```json
{"buildId":"...","locations":[{"page":2,"x":72,"y":204,"width":430,"height":12}],"sourceChanged":false}
```

page は 1 始まり．x / y は該当箇所を囲むボックスの左上で，ページの左上を原点とする．座標と width / height の単位は PDF point (72 point = 1 inch)．該当箇所がなければ HTTP 200 と `locations: []` を返す．コンパイルに使った原稿と現在の保存済み原稿が異なる場合は `sourceChanged: true` とし，その PDF を作った時点の行に基づく位置を返す．UI 内の未保存の編集については UI でも確認する．

空の結果には `reason` を添える．原稿が Input の一覧にない場合は `source-not-recorded`，CLI が該当箇所を返さなかった場合は `no-location` とする．

### POST /api/synctex/inverse

入力 `{ "buildId":"表示中の PDF の版", "page":2, "x":95, "y":210 }`．PDF 上でダブルクリックした座標を forward と同じ単位・原点で送る．

```json
{"buildId":"...","path":"章/原稿.tex","line":42,"column":0,"sourceChanged":false}
```

path は `/` を使うプロジェクト内の相対パス．line は 1 始まり．column は 1 始まりで，不明の場合は 0．プロジェクト内の原稿に対応する位置がなければ HTTP 404．プロジェクト外の TeX パッケージのファイル等は返さない．

両方の API は共通の token / Origin / プロジェクトの確認を受ける．削除済みの版は 410，SyncTeX データが未生成の場合は 404，synctex 実行ファイルが見つからない場合は 503，実行の失敗は 502，6 秒を超えた場合は 504．失敗時は `{ "error":"説明" }` を返し，CLI の実行に失敗した理由をコンパイルのログにも記す．

CLI は PDF を生成した TeX Live の bin にある `synctex` を，シェルを介さず実行する．Windows の文字コードによるファイル名の変化を防ぐため，キャッシュにコピーする際に SyncTeX の Input 名を ASCII の名前にし，アプリ内で元の相対パスへ対応付ける．エンジンが Input 名を Windows の文字コードに変換している場合は，原稿の名前にも同じ変換を適用して照合する．recorder (`.fls`) が記録した原稿を使って候補を絞り，同じ表現になる名前が複数残る場合はその Input への移動を行わない．Input の番号，行，ページと座標は元のデータを使う．PDF と SyncTeX のファイルには同じ版の名前を付け，次のコンパイルで元のファイルが書き換わっても，表示中の PDF のデータを使って照合する．

CLI の形式と座標は TeX Live の [synctex_main.c](https://github.com/TeX-Live/texlive-source/blob/trunk/texk/web2c/synctexdir/synctex_main.c) の `synctex help view` / `synctex help edit` と出力処理に基づく．forward のボックスは CLI の h / v / W / H から x = h，y = v - H，width = W，height = H とする (負の寸法は向きに合わせて左上と正の幅へ直す)．Input 名を置き換えた際の byte 数は [synctex.c](https://github.com/TeX-Live/texlive-source/blob/trunk/texk/web2c/synctexdir/synctex.c) の `synctex_record_anchor` に従って更新する．

[texmfmp.c](https://github.com/TeX-Live/texlive-source/blob/trunk/texk/web2c/lib/texmfmp.c) の `generic_synctex_get_current_name` は相対名へ working directory を前置するため，Input に `..././原稿.tex` が記録される．Windows では working directory が 8.3 形式になる場合もあるため，`GetShortPathNameW` で取得したプロジェクトのパスも照合の候補に加える．照合では文字コードを復元した後に `/./` 等を整理する．CP932 の後半 byte にある `0x5c` をパスの区切りとして扱わない．

### GET /api/session

編集タブごとに認証付きの `fetch` で開く．応答は `text/event-stream` とし，開始時と 15 秒ごとにコメントを送る．タブが閉じると HTTP の接続が切れ，最後の接続から 5 秒経過した時点でコンパイルとアプリを終了する．その間にページの再読み込み等で接続が戻ると，終了の予定を取り消す．

`beforeunload` の確認を取り消した場合や，タブが背景へ移った場合は接続を続ける．`pagehide` で接続を閉じ，`pageshow` で再開する．接続の一時的なエラーでも再接続を試みる．通常の `--no-browser` で起動し，一度もこの API を利用していないプロセスには，自動終了の期限を設けない．

通常の GUI 起動と旧版からの移行では，初回の編集画面の接続を 30 秒待つ．画面が開かれる前にタブを閉じた場合も，その期限でアプリが終了する．一度接続した後は，最後のタブの切断から 5 秒という上記の動作へ進む．

### POST /api/shutdown

コンパイルを停止し，`{"ok":true}` を返してサーバーを終了する．UI は未保存の編集がある場合に確認し，保存または草稿の記録を済ませてから呼ぶ．成功後に `window.close()` を呼び，ブラウザがタブを残した場合は終了画面へ切り替える．

### POST /api/shortcuts

Windows で起動用のショートカットを作成する．入力は `{ "destination":"start-menu" }` または `{ "destination":"desktop" }`．実行ファイルをユーザーの LocalAppData の `Programs\LocalTeXEditor\local-tex-editor.exe` に配置し，選んだ Known Folder へ `Local TeX Editor.lnk` を作る．成功時は `{ "executable":"...", "shortcut":"...", "message":"..." }` を返す．UI は設定画面のボタン操作から呼ぶ．

配置先とショートカット名はアプリで固定する．アプリ自身が配置した旧バージョンは更新できる．起動している旧バージョンのファイルを置き換えられない場合は，終了してから再度実行する案内を返す．

ショートカットのアイコンはアプリの設置先に保存する．ファイル名に画像の SHA-256 の先頭 16 桁を含め，画像が更新された際に Explorer が新しい画像を読み込めるようにする．既存のアプリ用ショートカットも再作成し，専用アイコンへ更新する．

通常の GUI 起動でも，アプリのロックを取得した後に配置済みのアプリと作成済みのショートカットを更新する．起動時の更新は既存の `installation.json` を確認してから行う．ポータブル版の初回起動ではインストール先やショートカットを作らない．

### GET /api/storage?key=文字列

UI の設定や草稿を `{ "value":..., "serialized":"JSON の文字列", "revision":"sha256" }` として読み込む．`revision` は `serialized` の UTF-8 bytes の SHA-256．未登録の場合は `{ "value":null, "serialized":"", "revision":"" }`．キーはパスとして扱わず，SHA-256 をファイル名にして設定フォルダーの `ui-state` に保存する．アプリの次回起動で localhost のポートが変わっても，同じキーから読み込める．

### PUT /api/storage

入力 `{ "key":"文字列", "value":..., "baseRevision":"読込時の revision" }`．`baseRevision` を指定した場合，現在の値と一致するときに書き込む．古い revision には HTTP 409 と現在の値を返す．成功時の形式は GET と同じ．JSON の上限は 16 MiB．同じフォルダーの一時ファイルに書き込み，完成した内容で置き換える．token，Origin，Host の確認は他の API と共通．`X-Project-Root` は参照しないため，プロジェクトを切り替えた直後も元のプロジェクトのキーへ草稿を保存できる．

### PATCH /api/storage

タブを閉じる際の小さな変更は，`fetch` の `keepalive` を使って送信する．長い原稿の全文がブラウザの送信サイズを超える場合にも，変更した部分だけを送れる．

```json
{"key":"drafts:...","patches":[{"baseRevision":"sha256","prefixBytes":100,"suffixBytes":200,"insert":"変更後の文字列"}]}
```

`prefixBytes` と `suffixBytes` は，保存済みの `serialized` の先頭と末尾から引き続き使う UTF-8 bytes の数．その間へ `insert` を入れて JSON を復元する．複数の箇所を更新する場合は，候補の `edits` 配列へ `{ "prefixBytes":..., "suffixBytes":..., "insert":"..." }` を入れる．各範囲は同じ元の文字列を参照する．本文，更新日時，選択位置の変更をそれぞれ送れるため，離れた箇所が変わっても原稿全体の送信を必要としない．原稿の改行や日本語を含む文字列にも使える．UTF-8 の文字の途中を指定した場合，範囲が重なる場合，不正な JSON になる変更は HTTP 400．

`patches` には最大 2 個の候補を送れる．通常の PUT の応答をまだ受け取っていない場合に，最後に確認した revision と送信中の値に対応する revision を指定する．サーバーは現在の revision に一致する候補を使う．一致する候補がなければ HTTP 409 と現在の値を返す．後から到着した古い PUT も `baseRevision` の照合によって HTTP 409 となる．

UI は草稿の書込みにこの照合を使う．別のタブの草稿と競合した場合は自動の再送を止め，利用者の「再保存」で再開する．編集位置と表示設定は通常の PUT で最後の値を保存し，閉じる直前も送信量の範囲内で全文を送る．

### DELETE /api/storage?key=文字列

指定したキーの値を削除し，`{ "ok":true }` を返す．保存済みの原稿ファイルには変更を加えない．

## CLI と動作確認

- `--project PATH`，`--tex-bin PATH`，`--listen 127.0.0.1:0`，`--no-browser`，`--ready-file PATH`，`--config-dir PATH`．
- ready-file へ `{ "url": "http://127.0.0.1:port/", "token": "...", "pid": 123 }` を書く．同じ JSON を標準出力にも出す．
- 設定は OS のユーザー用設定フォルダーへ保存する．--config-dir はテスト用にも利用する．
- 同じ設定フォルダーで同じバージョンが起動済みの場合，通常の起動は既存の画面を開く．異なるバージョンが動いている場合は，それぞれの版を示す画面を開き，旧版の終了後に新しいプロセスがアプリのロックを取得する．コンパイルにはプロジェクトごとのロックを使い，異なる設定フォルダーからの同時起動でも生成物の競合を防ぐ．
- `--no-browser` と `--ready-file` を同時に指定した場合は，異なるバージョンが起動中でも，移行画面をブラウザで開かずに準備する．この段階の ready-file には `phase: "update"` を加える．通常の編集サーバーを起動すると，同じファイルをその接続情報で更新する．
- 起動時に main.tex を優先して候補を選ぶが，UI から選択を変更できる．
- Compile はプロジェクトのルートを working directory にして latexmk を実行する．既存の latexmkrc / .latexmkrc を使う．出力先は `-outdir=.local-tex-editor/build` とする．CLI に jobname の強制指定をしない．出力は main に対応する fls / fdb_latexmk と実際の PDF から見つけ，latexmk が成功したことを確かめる．
- 根拠のない No PDF を避けるため，実行の失敗，終了コード，PDF 探索結果を必ず build.error か build.log へ記す．
- 手書きの HTTP パーサーは使わず，Go 標準ライブラリを使う．

## 分担

- backend: internal/app 以下．アプリ状態，ファイル，設定，コンパイル，API．
- frontend: web/src 以下．CodeMirror，PDF.js，日本語の UI．
- tests: .github/workflows と tests 以下．Windows での TeX，ブラウザ，配布物の確認．
- root: cmd，internal/platform，web/embed.go，package.json，scripts，README，GitHub への反映．
