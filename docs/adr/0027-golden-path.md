# ADR 0027: ゴールデンパスは「他チーム向けのひな形」として置き、掲示板は分割しない —— ひな形はそれ自体が動くサービスにして、CI で毎回生成して捨てる

- ステータス: **採用 (実装済み・手元で実測済み)**。
  `templates/go-service` / `tools/new-service` / `make new-service` /
  `make verify-golden-path` / `make golden-path-kind` (2026-11-14)。
  CI の Golden Path ジョブは PR #73 で初めて CI 上で走り、通過した (181 秒)。
  PR #73 のレビューで 10 件の指摘を受けて直した (「レビューで見つかったこと」)
- 日付: 2026-11-14

## 背景

プラットフォームエンジニアリングでいう**ゴールデンパス** (paved road) は、
「推奨の道を、いちばん楽な道にしておく」仕組みのこと。強制はしない —— 強制するのは
ガードレール (ポリシー) の仕事で、別物になる。中身は多くの場合
**新しいサービスのひな形**で、CI・Dockerfile・API の生成・デプロイの設定が最初から揃っている。

このリポジトリには、ひな形の材料が揃っていた:

- `apps/go-api` の Dockerfile (dev / builder / prod の 3 段、prod は distroless)
- OpenAPI を唯一の正にして oapi-codegen でスタブを生成する流れ ([ADR 0002](0002-openapi-direction.md))
- k8s のマニフェストと、そこで踏んだ穴の記録 ([ADR 0026](0026-kubernetes-and-gitops.md))
- 版の固定と、版のずれの検査 (`make verify-go-version`)

ただし**前提とぶつかる決定がある。** [ADR 0004](0004-modular-monolith.md) は
掲示板を**モジュラモノリス**として作り、Go モジュールを分ける案 (選択肢 D) を
「この規模では要らない」として退けている。`make new-service` で `apps/<name>/` に
新しいサービスを生やしてコミットすれば、この決定と正面からぶつかる。

**ひな形を何のために作るのか**を先に決める必要があった。

## 検討した選択肢

### 何を生成するか

#### A. 他チーム向けの新サービスのひな形 (採用)

- 利点: ゴールデンパスの典型的な形。CI・Dockerfile・OpenAPI の生成・k8s まで一式を見せられる
- 利点: **生成物をこのリポジトリにコミットしない**ので、掲示板は分割されず ADR 0004 と両立する
- 欠点: このリポジトリの中に、ひな形の「利用者」がいない。使われない仕組みは腐る
  —— 決定 3 で、CI が毎回利用者の代わりをする

#### B. go-api の中に新しい機能モジュールを生やす (`make new-module`)

- 利点: ADR 0004 と最も素直に整合し、このリポジトリで実際に使われる
  (`internal/<name>/{domain,usecase}` と go-arch-lint の宣言を足す)
- 欠点: Dockerfile も CI も増えない。「新しいサービスを作る道」としては語れない
- 欠点: モジュールは 6 つで、今後大きく増える予定が無い。ひな形にする回数が少ない

#### C. 両方

- 欠点: 学習計画の「やらないこと: 全部を浅く触ること」に最も近づく

### どう生成するか

| 道具 | 特徴 | 今回 |
| --- | --- | --- |
| Backstage Software Templates | 業界の代表格。UI から生成し、サービスカタログに登録までする | **見送り。** Backstage 本体 (Node のアプリ・DB) を運用することになる。利用者が自分 1 人の段階で背負う重さではない |
| cookiecutter | Python 製。Jinja2 のテンプレート | 見送り。ひな形が `{{ }}` だらけになり、**生成してみるまで壊れているか分からない** |
| copier | Python 製。Jinja2。**生成後の更新 (`copier update`) ができる** | 見送り。day-2 の更新を最初に解くべき規模ではない (「引き受けるコスト」)。覆す条件はそこに書いた |
| gonew (Go 公式・実験的) | **ひな形を普通に動く Go モジュールのまま持ち**、モジュールパスだけを書き換えてコピーする | 考え方を採用。ただし gonew 本体はモジュールパスしか書き換えないので、Dockerfile・k8s の名前に手が届かない |
| sh + sed | 依存ゼロ | 見送り。macOS と GNU で `sed -i` の書き方が違い、名前の検査も書きにくい |

## 決定 1: ひな形は他チーム向け。生成先はこのリポジトリの外

`templates/go-service` に置き、`make new-service NAME=... MODULE=...` は既定で
`../<NAME>` (リポジトリの外) に生成する。**掲示板は ADR 0004 のとおり分割しない。**

決め手は、ADR 0004 を覆す理由が無いこと。掲示板の分割は「ゴールデンパスを見せたい」
という理由で決めてよいことではない。ひな形の利用者は「このリポジトリの外のチーム」と置く。

## 決定 2: ひな形は、それ自体がビルドもテストも通るサービスにする

ひな形はテンプレート言語で書かない。`templates/go-service` は
モジュールパス `example.com/go-service`、名前 `go-service` の**普通のサービス**で、
そのまま `go test`・`golangci-lint`・`docker build`・`kubectl kustomize` が通る。

ジェネレータ (`tools/new-service`、標準ライブラリだけの Go) がやるのは:

1. 名前を検査する (k8s の名前に使えるか。英小文字で始まる 63 文字以内)
2. 空でない出力先には書かない (上書きしない)
3. 写すファイルは、ひな形が git の管理下なら `git ls-files --cached --others --exclude-standard`
   (git が無視するものは写さない。「レビューで見つかったこと」4)
4. ひな形をコピーしながら、`example.com/go-service` → `-module`、`go-service` → `-name` を
   **1 回の走査で**置き換える (罠 1)

決め手は、**ひな形そのものに lint とテストをかけられる**こと。
テンプレート言語で書くと、テンプレートのままではコンパイルできないので、
壊れたことに気づくのが「生成した日」になる。

## 決定 3: CI で毎回「生成して、検査して、捨てる」

ひな形は**使われない間に腐る。** 誰も `make new-service` を叩かない週にも、
依存の脆弱性は公表され、Go の版は上がる。

`make verify-golden-path` (CI の Golden Path ジョブ、`make check` にも入れた) は、
ひな形から `smoke-svc` を一時ディレクトリに生成し、**生成したサービスの側で**:

- `go.mod` / `go.sum` が tidy か
- 生成物 (oapigen) が仕様書と一致するか
- `go vet` / `golangci-lint` / `go test -race`
- `govulncheck`
- `kubectl kustomize` が組み立てられ、`go-service` が 1 か所も残っていないか
- 本番用イメージがビルドできるか

を確かめ、全部消す。ジェネレータ自身のテスト (置き換え漏れ・入力の検査・
置き換えの網に掛からない書き方の検出) もここで回す。

**実際に腐っていた。** ひな形を作った初日、`go get` で入った `golang.org/x/net` が
v0.57.0 で、govulncheck が 4 件 (GO-2026-6603 / 6611 / 6612 / 6617、修正は v0.60.0) を出した。
検査を置かなければ、**最初に生成したサービスが、生成した瞬間から脆弱**だった。

`make verify-go-version` も、ひな形 (`ci.yml`・`Dockerfile`・`go.mod`) と
ジェネレータの `go.mod` まで見るように広げた。Go の版を上げる日に、ひな形だけ取り残されない。

## 決定 4: 教訓を既定値に焼き込む (k8s のマニフェストまで含める)

ゴールデンパスの価値の大半は、**先に踏んだ人の教訓を、次の人が最初から踏まないこと**にある。
ひな形には、このリポジトリで実際に踏んだものを既定値として入れた:

| 既定値 | 出どころ |
| --- | --- |
| `runAsUser: 65532` を数字で書く (Dockerfile の `USER` も数字) | ADR 0026「実装して分かったこと」2 |
| ルートは読み取り専用、`/tmp` だけ `emptyDir` で書ける | ADR 0026「レビューと CI で見つかったこと」1 |
| liveness は依存先を見ない (`/healthz`)、readiness だけが見る (`/readyz`) | `apps/go-api` の `GetHealthz` |
| Go の版を patch まで固定し、3 か所を揃える | PR #71 |
| 仕様書でリクエストを検証し、仕様に無いメソッドは 405 | ADR 0002 / `apps/go-api` の `respondSpecError` |
| golangci-lint は配布バイナリではなく `go install` で入れる | `.github/workflows/ci.yml` の go-ci |
| SIGTERM の前に `preStop` で 5 秒待ち、処理中のリクエストを最大 20 秒待つ (計 25 秒 < `terminationGracePeriodSeconds` 30 秒) | 新規。レビュー指摘で `preStop` を足した (「レビューで見つかったこと」5) |
| oapi-codegen は `go.mod` の `tool` ディレクティブで固定 (`go tool oapi-codegen`) | 新規。利用者が道具を入れなくて済む |

**Argo CD の Application は含めない。** ひな形の利用者がどの GitOps の道具を使うかは
決められない。`deploy/k8s` は `kubectl apply -k` で入る素の kustomize にとどめ、
イメージのタグは `kustomization.yaml` の `images` で差し替える形にした (CD が書き換える場所を 1 つにする)。

## 決定 5: 効果は「生成から Ready まで」の秒数で測る

ゴールデンパスの効果を表す、よく使われる指標が **time to first deploy**
(作り始めてから最初に動くまで) になる。`make golden-path-kind` が、
素の kind クラスタ (`golden-path`、掲示板の `bbs` とは別) に対して
「生成 → イメージのビルド → kind への読み込み → apply → 2 台とも Ready」を測る
(クラスタの作成は含めない)。

| 回 | 秒数 | 状態 |
| --- | --- | --- |
| 1 | **35** | Docker の依存レイヤ (`go mod download`) は直前の `verify-golden-path` でキャッシュ済み。ノードにイメージは無い |
| 2 | 5 | ビルドのキャッシュ・ノード上のイメージとも残っている |
| 3 | 4 | 同上 |

レビュー対応のあと (`preStop` を足し、クラスタを作り直した状態) にもう一度測ると 26 秒だった。
冷え方の条件を揃えていないので、35 と 26 の差に意味は無い。

**完全に冷えた状態 (Docker のビルドキャッシュなし) は測っていない** (「まだ確かめていないこと」)。

比べる相手は「ひな形なしで同じものを書く時間」になるが、これは測れない
(自分 1 人で、もう答えを知っている)。数字が言えるのは**「35 秒で、
非 root・読み取り専用・プローブ・停止の作法が揃ったサービスが動く」**ところまで。

## 罠

### 1. 置き換えを 2 回に分けると、置き換えた結果をもう一度置き換える

`-module github.com/acme/go-service-x` のように、モジュールパスに `go-service` を含むと、
「モジュールパスを置き換える → 名前を置き換える」の 2 段では 2 段目が 1 段目の結果を壊す。
`strings.NewReplacer` は置き換え後の文字列を再走査しないので、1 回の走査で済ませる。
テスト (`TestGenerate_ModuleContainingPlaceholderName`) で固定した。

### 2. 生成物に仕様書が埋め込まれている —— 仕様書にサービス名を書けない

`embedded-spec: true` で、仕様書は圧縮されて `oapigen.gen.go` に入る。
`info.title` に `go-service` と書くと、ジェネレータが仕様書を置き換えた直後に
**埋め込まれた方とずれて、生成したサービスの `verify-generated` が落ちる。**
タイトルは `Service API` にしてある (仕様書の冒頭にコメント)。

### 3. 置き換えの網に掛からない書き方

ジェネレータが置き換えるのは `go-service` だけ。ひな形に `GoService` や `go_service` と
書くと、生成したサービスに元の名前のまま残る。生成結果の検査は `go-service` しか探さないので、
`TestTemplate_HasNoUnreplaceableVariants` がひな形の側で 6 つの書き方を禁じる。

### 4. 検証ミドルウェアは、仕様に無いメソッドを 400 で返す

`OapiRequestValidator` は `POST /hello` (仕様は GET のみ) を **400** にする (実測)。
「パラメータが悪い」と区別できないので、`routers.ErrMethodNotAllowed` を見て 405 に直した。
go-api はこれを既に `respondSpecError` で直していた —— 見本から写すときに落ちる種類の知識で、
ひな形のテストに入れて初めて気づいた。

### 5. `verify-generated` を `git diff` で書くと、`git init` 前に誤って落ちる

生成した直後のディレクトリはまだ git の管理下に無い。`git diff --exit-code` は
「リポジトリではない」で非 0 を返し、それを「生成物がずれている」と報告してしまう。
一時ディレクトリに作り直して `diff -u` で比べる形にした (git に頼らない)。

## 検査の穴 (わざと壊して見つけたもの)

[ADR 0022](0022-probing-the-checkers.md) の流儀で、検査を置くたびに**わざと壊して落ちるか**を見た。
壊した 4 種 (go.mod を tidy でなくする / 仕様書だけ変える / ひな形に `go_service` を混ぜる /
4 か所の Go の版を 1 つずつずらす) のうち、**最初の実装では 3 か所で検査が正しく動いていなかった** (2 か所は壊しても通り、1 か所は壊さなくても 2 回目から落ちた)。

### 1. golangci-lint の共有キャッシュは、消えた一時ディレクトリのパスを返す

1 回目の `verify-golden-path` は通り、**2 回目から落ちた。** 共有のキャッシュが、
1 回目の (もう消えた) 一時ディレクトリのパスつきで結果を返し、生成コードを除外する
フィルタがファイルを開けずに素通しした (`failed to get doc ... no such file or directory`、
生成コードの ST1005 がそのまま出る)。`GOLANGCI_LINT_CACHE` を一時ディレクトリの中に置いた。

### 2. go test は、モジュールの外のファイルの変化でキャッシュを捨てない

ひな形に `go_service` を混ぜても、ジェネレータのテストは **`ok (cached)`** を返した。
テストが読むひな形は `tools/new-service` モジュールの外にあり、go test は
モジュール外のファイルをキャッシュの鍵に入れない。`-count=1` にした。

### 3. Makefile のシェルの中に `#` を書くと、そこから後ろが全部コメントになる

`verify-go-version` を広げたとき、シェルの途中に `# ... \` とコメントを書いた。
make は行末の `\` で 1 行につなげてシェルに渡すので、**`#` から後ろ —— 元からあった
検査を含めて —— が丸ごとコメントになり、版をずらしても exit 0 で通った。**
`OK` の表示すら出なかったことで気づいた。コメントはシェルの外 (レシピの先頭) に出した。

## レビューで見つかったこと (PR #73、2026-11-14)

10 件。重い順に。

### 1. `kill $${pf:-0}` は、pf が空のときプロセスグループ全体を殺す

`golden-path-kind` の後始末が `kill ${pf:-0}` だった。port-forward より前で落ちると
`kill 0` になり、**make 自身を含むプロセスグループ全体に SIGTERM が飛ぶ。**
本来の失敗の出力が `Terminated` に埋もれる。pf が立っているときだけ kill するようにし、
Dockerfile をわざと壊して、本来の原因 (`go mod: unknown command`) がそのまま見えることを確かめた。

### 2. 疎通の確認が失敗しても OK を出していた

curl の再試行が 20 回とも失敗しても、ループを抜けて「OK: 生成から Ready まで N 秒」を出していた。
**計測の値が、疎通していない状態のものになりうる。** 応答の中身まで突き合わせ、違えば NG にした
(`/hello` の応答をわざと変えて、NG になることを確かめた)。

### 3. ジェネレータの既定値で動く場所が無かった

doc コメントはリポジトリ直下からの `go run ./tools/new-service` を案内していたが、
直下には go.mod も go.work も無いので動かない。中に入って叩くと、今度は既定の
`-template` (`templates/go-service`) が見つからない。`make new-service` は値を明示していたので
気づかなかった。既定値を `tools/new-service` から見た相対パスにし、そこで生成できることを確かめた。

### 4. 未追跡のファイルまで写していた

ディレクトリを丸ごと歩いていたので、Finder が作る `.DS_Store`、手元の `.env`、ひな形の中で
試しに作ったバイナリまで写す (バイナリはエラーになり、テキストは黙って写る)。
**CI はクリーンな checkout なので、この経路は検証網に掛からない。**
`git ls-files --cached --others --exclude-standard` で写すものを決めるようにした。
未追跡でも無視されていないファイルは写す (ひな形に足した直後、コミット前に試せるように)。

### 5. SIGTERM ですぐにリスナを閉じると、ローリング更新の間に新しい接続が拒否される

k8s は Pod を消すとき、SIGTERM の送信と、Service の振り分け先 (EndpointSlice) から外す処理を
**並行に**走らせる。すぐにリスナを閉じると、外れる前に届いた接続が拒否される。
「ローリング更新で処理中のリクエストを落とさない」と書いていたが、守れていたのは処理中のものだけで、
**新しく来るものは守れていなかった。**

`lifecycle.preStop.sleep` (5 秒) を足した。distroless にはシェルが無いので、
`exec: sleep` の書き方は使えない。kind (v1.37) で Pod を消し、
削除の開始から `shutting_down` のログまで **5.1 秒**空くことを確かめた。

### 6. ひな形のツールの版が見張られていなかった

ひな形の Makefile の `GOLANGCI_LINT_VERSION` はどこからも使われておらず、ひな形の `ci.yml` に
別の版が書いてあった。ルートの版を上げても、生成するサービスには古い版が固定され続ける。
ひな形の中では版を Makefile の 1 か所に寄せ (`ci.yml` は `make tools` / `make vulncheck` を呼ぶだけ)、
`make verify-template-versions` がルートの Makefile と突き合わせる。`checker-probe` に変異 X・Y を足した。

### 7. `GOVULNCHECK_VERSION` を Makefile の途中で定義していた

CLAUDE.md は「ツールのバージョンは Makefile 冒頭で固定」と約束している。冒頭の並びへ移した。

### 8. 検査が、ひな形の Makefile の手順を書き直していた

`verify-golden-path` が `go vet` / `golangci-lint` / `go test` をその場で書いていた。
ひな形の Makefile の手順を変えると「CI と同じ検査を通る」がずれる。
生成したサービスの `make lint` / `make test` / `make vulncheck` を呼ぶようにした。

### 9. Golden Path ジョブが golangci-lint を毎回ビルドしていた

初回の CI で 50 秒かかった。go-ci と同じ鍵でツールのキャッシュを復元する (保存は go-ci に任せる)。

### 10. `make check` の前提が増えたのに書いていなかった

Docker とネットワーク (`go mod tidy`、govulncheck の DB) が要るようになった。help に書いた。

## ほかに見つかったこと

- **リポジトリの CI は govulncheck を `@latest` で入れている** (go-ci の
  「Run Vulnerability Check」)。理由は「脆弱性データベースは新しいほうが良い」だが、
  **データベースはツールの版と無関係に、実行のたびに `https://vuln.go.dev` から取る**
  (`govulncheck -h` の `-db` の既定値で確認)。ツールを固定しても DB は古くならない。
  ひな形では `v1.9.0` に固定した。リポジトリ側は**この ADR では直していない** (別の PR で揃える)

## まだ確かめていないこと

- **Golden Path ジョブで、ツールのキャッシュが当たる経路は CI でまだ走っていない** (レビュー指摘 9 の修正後)
- **生成したサービスの `.github/workflows/ci.yml` は一度も走っていない。** actionlint は通した
  (v1.7.7、exit 0)。別のリポジトリに置いて push しないと確かめられない
- **完全に冷えた状態の time to first deploy** (決定 5)。`docker builder prune` は
  ほかのプロジェクトのキャッシュも消すので、手元ではやっていない
- **非 root・読み取り専用のルートが効いていることは、Pod の状態からの判断。**
  kubelet が `runAsNonRoot` を確かめて Running になったこと、SIGTERM で `shutting_down` が出て
  止まることは見た。コンテナの中から書き込めないことは確かめていない (distroless にシェルが無い)
- **ひな形を他人が使ったことは無い。** 利用者体験 (README だけで迷わず使えるか) は未検証

## 引き受けるコスト

- **day-2 の更新を持たない。** 生成したサービスは生成時点の写しで、ひな形を直しても届かない
  (README に明記した)。copier の `update` や、生成元の版を記録して差分を当てる仕組みが要る。
  覆す条件: ひな形から作ったサービスが 2 つ以上になり、片方にだけ直したいものが出たら、copier へ移す
- **見本 (`apps/go-api`) とひな形の 2 系統を保つ。** 教訓は go-api で見つかり、手でひな形に写す。
  写し忘れは検査できない (罠 4 は、たまたまテストで気づいた)。逆向きもある ——
  `preStop` はひな形にだけ入り、掲示板の `infra/k8s/bbs/go-api.yaml` には無い
  (レビュー指摘 5 と同じ穴が残っている。go-api は distroless なので同じく `sleep` で塞げる)
- **ひな形の依存を誰かが上げ続ける必要がある。** govulncheck が落ちたら、ひな形の `go.mod` を上げる。
  CI の Golden Path ジョブが赤くなることが合図になる
- **`make check` が Docker とネットワークを要するようになった** (help に明記)
- **手元の golangci-lint が Go 1.25 製だと `verify-golden-path` が落ちる。** go-api の `make lint` と同じ。
  `GOLANGCI_LINT=` で Go 1.26 製のものを渡す

## やらないこと

- **掲示板の分割** (決定 1)
- **Backstage の導入。** カタログと UI は、利用者が複数チームになってから
- **Argo CD の Application の生成** (決定 4)
- **DB つきのひな形。** 依存先を足す場所 (`/readyz`) だけ空けてある。sqlc とマイグレーションまで
  入れるのは、DB を使うサービスを 1 つ作ってから
- **DORA 指標** —— 学習計画では「余裕があれば」。このリポジトリは本番に常時出していない
  ([ADR 0024](0024-aws-deployment.md) 決定 1) ので、デプロイ頻度をそのまま数えられない
