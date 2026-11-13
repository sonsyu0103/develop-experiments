# ADR 0026: 掲示板を kind 上で動かし、Argo CD に Git どおり揃えさせる —— AWS の経路は ECS のまま

- ステータス: **採用 (実装済み・手元で実測済み)**。
  `infra/k8s` / `make k8s-up` / `make k8s-verify` (2026-11-13)。
  **Argo CD が作業ブランチ (`feat/k8s-gitops`) から同期し、`make k8s-verify` が
  全項目通った。** 既定の枝 (`develop`) から同期したことはまだ無い
  (下の「まだ確かめていないこと」)。PR #70 のレビューで 10 件の指摘を受けて直した
  (「レビューで見つかったこと」)
- 日付: 2026-11-13

## 背景

このリポジトリが持つ実行環境は 2 つだった:

| 環境 | 何で動かすか | 変更の届け方 |
| --- | --- | --- |
| 手元 | Docker Compose (`compose.yaml`) | `make up` |
| AWS | ECS Fargate ([ADR 0024](0024-aws-deployment.md)) | `make tf-deploy` / `deploy.yml` (**押し込む**) |

どちらも**命令で押し込む**形をしている。「いまクラスタに何が出ているか」は、
最後に誰が何を叩いたかで決まり、Git を読んでも分からない。

**Kubernetes と GitOps は 1 行も無かった。** 求人票では Kubernetes と
Argo CD / Flux の経験を求める記述が多く、ポートフォリオとして読まれる以上、
「マニフェストを書ける」ことと「Git を正にして環境を揃え続けられる」ことは
示しておきたい。

ただし [ADR 0024](0024-aws-deployment.md) の決定 1 (常時公開しない、
1 日 $1.3) を崩してまでやることではない。**手元で、課金ゼロで**やる。

## 検討した選択肢

### クラスタ

#### A. EKS

- 利点: 本番と同じ。IAM / ALB Controller / IRSA まで話ができる
- 欠点: **コントロールプレーンだけで $0.10/時 (月 $73)**。ADR 0024 の構成全体
  (月 $39) の倍になる。立てて壊す運用でも、作成に 10〜15 分かかる
- 欠点: 「Kubernetes を書ける」を示すのに、AWS の課金まで背負う理由がない

#### B. minikube / k3d

- 利点: どちらも手元で動く。k3d (k3s) は軽い
- 欠点: k3s は Kubernetes を独自に削った配布物で、
  **素の Kubernetes と挙動が違う部分がある** (組み込みの Traefik、
  SQLite を使う既定のデータストアなど)。何が Kubernetes の性質で何が
  k3s の都合かを読み手が切り分けにくい
- minikube は VM / ドライバの選択肢が多く、手元の環境差が出やすい

#### C. kind (採用)

- 利点: Kubernetes 自身の CI に使われている、素の Kubernetes をコンテナで動かすもの。
  **ノードイメージを digest で固定できる** —— Makefile 冒頭のツール固定と同じ考え方が取れる
- 利点: 消すのが `kind delete cluster` 1 つで、何も残らない
- 欠点: ノードはホストの Docker のイメージキャッシュを共有しない (実装して分かったこと 3)
- 欠点: LoadBalancer も Ingress Controller も無い。NodePort を
  `extraPortMappings` でホストへ出す

### GitOps

#### A. CI から `kubectl apply` (push 型)

- 利点: 部品が増えない。`deploy.yml` と同じ形で書ける
- 欠点: **CI に クラスタの認証情報を持たせる**ことになる。手元の kind には
  GitHub Actions から届かない
- 欠点: 手でクラスタを変えても誰も戻さない。「Git と違う」ことに気づく仕組みが無い

#### B. Flux

- 利点: Kubernetes のリソース (CRD) だけで完結し、構成が素直。
  イメージの自動更新 (image-automation-controller) が公式に揃っている
- 欠点: 既定で画面が無い。「Git とクラスタの差分」を見せる手段が CLI になる

#### C. Argo CD (採用)

- 利点: **差分と同期状態が画面で見える。** 「手で崩すと OutOfSync になり、
  selfHeal で戻る」を見せるとき、読み手が自分で確かめられる
- 利点: 採用例と求人での言及が多い。ポートフォリオは読まれてこそ
  ([ADR 0024](0024-aws-deployment.md) 決定 2 と同じ理由)
- 欠点: 部品が多い (server / repo-server / application-controller / redis /
  dex / applicationset / notifications の 7 つ)。手元のメモリを食う
- 欠点: Flux と比べて、イメージ自動更新は別部品 (Image Updater) になる

### マニフェストの書き方

#### A. Helm chart

- 利点: 値を差し替えて複数環境に出せる
- 欠点: **環境は 1 つ (kind) しかない。** テンプレートの `{{ }}` が読み手の
  負担になるだけで、差し替える値が無い

#### B. kustomize (採用)

- 利点: kubectl にも Argo CD にも組み込まれていて、追加の道具が要らない
- 利点: 素の YAML のまま読める
- 欠点: glob を受け付けない (決定 3)

## 決定 1: kind のノード 1 台で、版をすべて固定する

| 道具 | 版 | 固定している場所 |
| --- | --- | --- |
| kind | v0.33.0 | `Makefile` (`KIND_VERSION`) |
| ノード (Kubernetes) | v1.37.0 (digest まで) | `infra/k8s/kind-cluster.yaml` |
| kubectl | v1.37.0 | `Makefile` (`KUBECTL_VERSION`) |
| Argo CD | v3.5.6 | `Makefile` (`ARGOCD_VERSION`) と `argocd/kustomization.yaml` の URL |

kind と kubectl はリポジトリの `bin/` (gitignore 済み) に、**ファイル名に版を入れて**
(`bin/kind-v0.33.0` / `bin/kubectl-v1.37.0`) 置く。版なしの名前だと、
`KIND_VERSION` を上げても「もうある」と見なされて入れ直されない (レビューで見つかったこと 5)。
**手元にあった kubectl は v1.25 で、v1.37 のクラスタとは保証範囲 (±1) の外**だった。
各自の PC に入っている版に任せると、人によって挙動が変わる。

Argo CD は公開から 2 週間を過ぎた版を選んだ (v3.6.x は出て 8 日だった)。
Argo CD の版は 2 か所に書くことになるので、`make k8s-lint` が突き合わせる。

**ノードは 1 台。** 台数を増やしても、この PC 1 台の CPU とメモリを分け合うだけで、
検証できることは増えない。

**kubectl には必ず `--context kind-bbs` を付ける** (Makefile の `KUBECTL`)。
素の kubectl は「いま選ばれているクラスタ」に向かうので、
別のクラスタを触っていた日に、手元用の操作が他所へ飛ぶ。

## 決定 2: Argo CD は自動同期 + prune + selfHeal。Argo CD 自身は管理させない

`infra/k8s/argocd/application.yaml`:

- **automated + prune + selfHeal。** Git から消したものはクラスタから消え、
  手でクラスタを変えても Git の状態へ戻る。selfHeal を外すと
  「OutOfSync と表示するだけで戻さない」ことを実測した (実機で確かめたこと)
- **見る枝は `develop`。** PR がマージされた枝にクラスタが追従する。
  作業ブランチを試すときは `make k8s-up K8S_REVISION=feat/xxx` で、
  **Git を書き換えずに**上書きする

  **初版は `main` にしていた**(「何が出ているか」を main で読めるように)。
  だが main への反映はリリースの PR (develop → main) を待つので、
  この PR をマージした直後の `make k8s-up` は「main に `infra/k8s` が無い」で
  同期できず、600 秒待ってタイムアウトする (レビューで見つかったこと 2)。
  このクラスタは手元の開発用で、本番 (main → ECS) とは役割が違う。
  main を見る理由は「本番相当」の位置づけから来ていたが、それは ECS が担っている
- **Argo CD 自身は Argo CD に同期させない** (`make k8s-argocd` で kubectl apply する)。
  Argo CD が自分の設定を同期して壊れたら、直す手段も一緒に壊れる。
  App of Apps で Argo CD 自身まで載せるのは、複数クラスタを持ってから考える

## 決定 3: 設定ファイルはリポジトリの正本を直接読む (コピーしない)

マイグレーション (`apps/go-api/db/migrations`)、拡張の初期化 (`db/init`)、
エッジの `Caddyfile` は、**compose と同じファイルを kustomize の
`configMapGenerator` で読む。**

コピーを `infra/k8s` に置くと、片方だけ直す事故が必ず起きる。
`api/openapi.yaml` を単一の情報源にしているのと同じ扱いにする。

代わりに 2 つ背負う:

1. kustomization のあるディレクトリの外を読むので、**読み込み制限を外す**
   必要がある。手元 (`make k8s-render`) と Argo CD (`argocd-cm` の
   `kustomize.buildOptions`) の**両方で**同じ指定をする (実装して分かったこと 1)
2. kustomize は glob を受けないので、**マイグレーションの一覧を手で並べる。**
   足し忘れると k8s だけ古いスキーマで動き、エラーはどこにも出ない。
   `make k8s-lint` (CI の `k8s-ci` と `make check`) がディレクトリと突き合わせて落とす。
   1 本消して落ちることを確かめた

ConfigMap の名前には中身のハッシュが付く。中身が変わると参照する Pod / Job も
作り直されるので、「ConfigMap だけ新しく、Pod は古い設定のまま」が起きない。

## 決定 4: イメージは手元でビルドして kind に直接読み込む。Git にはタグを載せない

`make k8s-images` が go-api / next-app の `prod` ステージをビルドし、
`kind load docker-image` でノードへ入れる。マニフェストは `:local` を
`imagePullPolicy: Never` で指す。

**これで GitOps は半分になる。** Git が決めているのは「どう動かすか」
(マニフェスト) だけで、「何を動かすか」(イメージの中身) は手元の
最後のビルドで決まる。Git を読んでも、動いているコードがどのコミットか分からない。

採らなかった案と、採らなかった理由:

| 案 | 採らなかった理由 |
| --- | --- |
| kind の隣にローカルレジストリを立て、コミット SHA でタグを打つ | タグを書き換えるコミットを誰が作るか (CI か Image Updater か) まで決めないと閉じない。今回の主題 (Git どおりに揃う) の外 |
| Argo CD Image Updater | 部品がもう 1 つ増え、レジストリが前提になる |
| GHCR に push して引かせる | kind から外へ引きに行く形になり、「手元で課金ゼロ・ネット無しでも動く」が崩れる |

**覆す条件:** 手元以外のクラスタ (EKS など) に出す日が来たら、
レジストリ + SHA タグ + タグ書き換えのコミットを入れる。そのときは
Flux の image-automation と Argo CD Image Updater を比べ直す。

## 決定 5: Secret は平文で Git に置く (値は compose と同じ手元専用の既定値)

`infra/k8s/bbs/secret.yaml` の値は `password` / `minioadmin` で、
compose.yaml に既に平文で書かれているものと同じ。守る対象が無い。

**本物の秘密を載せる日が来たら、ここは Git に置けない。** そのときは
Sealed Secrets (Git に暗号化して置く) か External Secrets Operator
(Secrets Manager から引く。ADR 0024 決定 7 と繋がる) に置き換える。
Google OIDC の client secret を入れたくなった時点が、その日になる。

## 決定 6: 起動順は sync-wave で付け、初期化は Sync フックの Job にする

| 波 | 中身 |
| --- | --- |
| -1 | Secret |
| 0 | postgres / minio / mailpit |
| 1 | migrate / minio-init (Job。Sync フック) |
| 2 | go-api / next-app / edge |

Argo CD は前の波が Healthy になるまで次へ進まない。compose の
`depends_on: condition: service_healthy` に当たる。

**Job は普通のリソースとして置かない。** Job の `spec.template` は作成後に
変えられないので、マイグレーションを 1 本足しただけで (ConfigMap 名の
ハッシュが変わり) 同期が `field is immutable` で止まる。Sync フック +
`BeforeHookCreation` にして、同期のたびに作り直して流す。`migrate up` は冪等なので困らない。

## 決定 7: AWS の経路は ECS のまま。k8s は置き換えではない

ECS ([ADR 0024](0024-aws-deployment.md)) を EKS に移すことはしない。
この規模でコントロールプレーンに月 $73 を払う理由がなく、ADR 0024 の
決定 1 (立てて壊す、1 日 $1.3) と両立しない。

k8s に**持ち込まなかったもの**:

- **ログ基盤 (fluent-bit → S3)。** compose は Docker の fluentd ログドライバで
  集めているが、k8s には同じものが無い。DaemonSet で fluent-bit を置けば
  同じ形にできる ([ADR 0010](0010-log-pipeline.md))。主題の外なので今回はやらない
- **TLS。** エッジ (Caddy) は http で受ける。https の分岐 (`X-Forwarded-Proto`・
  `Secure`・`TRUSTED_PROXIES`) は compose 側の `make https-verify` が
  実測している ([ADR 0023](0023-local-edge-and-https.md))
- **seed。** compose でも既定では走らせない (profiles) のと同じ

## 実装して分かったこと (2026-11-12〜13)

踏んだ順に並べる。**どれも `kubectl kustomize` が通った後に起きた。**

### 1. kustomize はディレクトリの外を読めない —— 手元と Argo CD の両方で外す

```
error: loading KV pairs: file sources: [Caddyfile=../../caddy/Caddyfile]:
security; file '.../infra/caddy/Caddyfile' is not in or below '.../infra/k8s/bbs'
```

決定 3 の代償。`--load-restrictor LoadRestrictionsNone` で外す。

**怖いのは片方だけ外した状態。** Makefile にだけ付けると、手元の
`make k8s-render` は通るのに Argo CD だけが ComparisonError で止まる。
`argocd-cm` の `kustomize.buildOptions` にも同じ値を入れる。この設定は
repo-server が起動時に読むので、`make k8s-argocd` は最後に repo-server を作り直す。

### 2. `runAsNonRoot` は、USER が名前のイメージを起動させない

```
Error: container has runAsNonRoot and image has non-numeric user (nonroot),
cannot verify user is non-root
```

go-api (distroless、`USER nonroot:nonroot`) と next-app (`USER nextjs`) の両方で
`CreateContainerConfigError` になった。**イメージ側は正しく非 root で動く**のに、
kubelet は名前から UID を引けないので「root でないと確かめられない」と拒否する。

`runAsUser` に数字を書いて直した (distroless の nonroot は 65532、
next-app は Dockerfile の `adduser -u 1001`)。

### 3. kind のノードは、ホストのイメージキャッシュを共有しない

postgres の Pod が **12 分たっても `Pulling image "postgres:17-alpine"`** のままだった。
ホストの Docker には compose で使った同じイメージがあるのに、
kind のノードは別のコンテナランタイム (containerd) なので、Docker Hub から引き直していた。

その間、migrate の Job は postgres に繋がらず失敗を重ね、やり直しの間隔が
指数的に伸びていった。`backoffLimit` を使い切れば Job ごと Failed になり、
Argo CD 経由なら同期が止まる。

2 つ直した:

- `make k8s-images` で、**マニフェストに出てくる他所のイメージもホストから読み込む。**
  一覧はマニフェストから引く (Makefile に二重に書かない)
- Job の initContainer で **postgres / minio が受け付けるまで待ってから**流す。
  待ち合わせを Job のやり直しに任せない

### 4. `kubectl apply` は順序を守らない。headless Service だと「名前が無い」と出る

Argo CD を通さずに `kubectl apply -k` で入れたとき、go-api が **4 回再起動**した:

```
postgres: 疎通確認に失敗しました: ... hostname resolving error:
lookup postgres on 10.96.0.10:53: no such host
```

postgres が Ready になる前に go-api が立った。それ自体は再起動で治るが、
**エラーが「名前の書き間違い」に見える**のが問題だった。postgres の Service を
headless (`clusterIP: None`) にしていたため、Ready な Pod が居ない間は
DNS が名前ごと返さない。普通の ClusterIP にすると、名前は常に引け、
失敗は接続拒否として出る。

**Argo CD 経由 (sync-wave あり) で空から入れ直すと、go-api の再起動は 0 回だった。**
Pod の作成時刻も 波 0 (32 秒前) → 波 1 (22 秒前) → 波 2 (18 秒前) の順に並んだ。

### 5. Argo CD の CRD は、client-side apply では入らない

```
The CustomResourceDefinition "applicationsets.argoproj.io" is invalid:
metadata.annotations: Too long: may not be more than 262144 bytes
```

client-side apply は適用した内容をまるごと `last-applied-configuration`
注釈に書き込むので、大きい CRD は注釈の上限 (256KiB) を超える。
`--server-side --force-conflicts` で入れる。

**Application は CRD と同じ apply に入れない。** CRD が登録される前に
Application を送ると `no matches for kind` で落ちる。`make k8s-app` を別にしてある。

### 6. Service のパッチで `protocol` を省くと、`nodePort` だけが黙って落ちる

Argo CD の画面を NodePort 30443 で出すパッチを当てたら、**`type: NodePort` は
効いたのに nodePort はランダムな番号 (31046) になった。** kind の
`extraPortMappings` には繋がらず、`curl` は空応答 (exit 52) を返した。

Service の ports は `port` と `protocol` の組で突き合わせて合成される。
パッチ側に `protocol: TCP` が無いと別の要素と見なされ、上書きが半端に効く。
エラーも警告も出ない。

### 7. 同期の間隔には、設定とは別に「揺らぎ」が乗る

`timeout.reconciliation: 60s` にしたのに、push から同期まで **123 秒**かかった。

v3.5.6 の設定例によると、既定は 120 秒で、それとは別に
`timeout.reconciliation.jitter` (既定 60 秒) が足される。間隔を 60 秒にしても
実際は 60〜120 秒になる。揺らぎは Application が多いときに repo-server への
問い合わせを分散させるためのもので、1 つしか無いここでは要らない。
0 にして測り直すと **51 秒**だった (どちらも 1 回ずつの測定)。

最初に書いたコメントは既定を「3 分」としていた。**公式の設定例を読まずに
書いた数字で、間違いだった。**

### 8. 検査の文言が、検査していないことを主張していた

最初の検査は、書き込みが 201 で通ったときに
「TRUSTED_PROXIES と XFP が噛み合っている」と表示していた。

**この 201 は X-Forwarded-Proto について何も言っていない。** http の構成では
XFP が無くても scheme は http のままで、しかも Origin は CORS の許可リストで
先に通る ([ADR 0023](0023-local-edge-and-https.md) の https-probe が
「エッジ経由では測れない」と書いているのと同じ理由)。表示を
「DB まで書ける」「CSRF 検査が効いている」に直した。

### 9. 小さいもの

- **bash で `$code。` と書くと、全角の句点まで変数名として読まれる**
  (`code�: unbound variable`)。日本語の直前の変数は `${code}` と括る
- **Application の finalizer に Kubernetes v1.37 が警告を出す**
  (`prefer a domain-qualified finalizer name including a path`)。
  Argo CD が認める `/` 付きの名前は `.../background` だけで、削除の方式が変わる。
  警告のために挙動は変えず、公式どおりの名前のままにした
- **Docker Desktop が起動しておらず、kind も入っていなかった。** 決定 1 の
  `bin/` への版固定は、ここから入れたもの

## レビューで見つかったこと (PR #70、2026-11-13)

レビューの指摘 10 件をすべて直した。**直す前に、再現できるものは再現した。**

### 1. 読み取り専用のルートでは、1 MiB を超える画像を上げられない

go-api は `readOnlyRootFilesystem: true` にしていた。画像のアップロードは
1 MiB を超えた分を `/tmp` に一時ファイルとして書く (`images.go` の
`multipartMemoryLimit`。上限は 5 MiB)。**compose と ECS はルートを読み取り
専用にしていないので、k8s でだけ壊れる。**

DB にテスト用のセッションを直接作り、3 MiB の PNG を上げて再現した:

```
直す前: 400 {"message":"... open /tmp/multipart-1586701104: read-only file system"}
直した後: 201、エッジ経由で取り出して 200 (WebP 722KB)
```

`/tmp` に emptyDir (上限 64Mi) を足して直した。最初の検査は画像を上げていなかったので、
**全項目 OK の裏でこれが壊れていた。**

### 2. 既定の枝 `main` には、マニフェストが無い

決定 2 を参照。既定を `develop` に替え、さらに `make k8s-app` は
**その枝にマニフェストがあるかを最初に確かめる**ようにした。
main を指させると、600 秒のタイムアウトではなく 1 秒で
「main (90c07db) に infra/k8s/bbs が無い」と落ちる。

### 3. 枝を替えた直後の「Synced」は、前の枝の Synced

`make k8s-app` は Synced と Healthy を待っていた。枝を替えた直後は
Argo CD がまだ新しい枝を見ておらず、**前の枝のまま Synced / Healthy でいる**ので、
待ちは即座に通り、クラスタが前の枝のままなのに成功を報告する。

**同期したコミットが枝の先頭と一致し、かつ Synced / Healthy** の 3 つが
揃うまで待つようにした (`.github/scripts/k8s-sync-wait.sh`)。
新しいコミットを push した直後に流すと、そのコミットに揃うまで待ってから通った。

### 4. postgres の readiness は、ソケット越しだと初期化中も通る

compose の healthcheck は「`pg_isready` だけだと初期化中も通るので
`SELECT 1` を足す」と書いている。k8s 側はそれすら無かった (指摘)。

**だが compose のやり方でも防げていなかった。** init スクリプトを
わざと 20 秒止めて、2 秒ごとに測った:

| 経過 | ソケット越しの `pg_isready && SELECT 1` | TCP 越しの `pg_isready -h 127.0.0.1` |
| --- | --- | --- |
| 2〜18 秒 (初期化中) | ok | ng |
| 20 秒〜 (初期化完了) | ok | ok |

postgres イメージは初期化中、**TCP を閉じた一時サーバをソケットだけで**立てる。
ソケット越しの確認は、何を足しても初期化中に通る。

さらに **`pg_isready` は存在しない DB を指しても 0 を返す** (実測。「接続を受け付けるか」
しか見ない)。k8s 側は **TCP 越しの `psql SELECT 1`** にした —— 初期化中は通らず、
ロールと DB が無ければ落ちる。127.0.0.1 は pg_hba で trust なのでパスワードは要らない。

**compose の healthcheck は別の PR (#72) で同じ形に直した。** この ADR の初版は
「compose は db-init / migrate の待ち合わせが別にあるので実害は出ていない」と
書いていたが、**誤りだった** (#72 のレビュー指摘)。db-init は postgres の healthcheck
だけを待ち、自前の待ちも再起動も無い。実害が出なかったのは、初期化 (拡張 2 つ) が
最初の確認 (5 秒後) より先に終わっていた偶然による。k8s 側は db-init を
initContainer の中で TCP 越しに待たせている (実装して分かったこと 3) ので、
この偶然には頼っていない。

### 5. その他

| 指摘 | 直し方 |
| --- | --- |
| Argo CD なしの `k8s-apply-local` では、初期化の Job が `field is immutable` で落ちる (マイグレーションを足すと ConfigMap 名が変わるため) | 入れ直す前に Job を消す。`db/init` の中身を変えて 2 回流し、2 回目も通ることを確かめた |
| ツールのファイル名に版が無く、版を上げても入れ直されない | ファイル名に版を入れた (決定 1) |
| `k8s-up` が前提の並び順に頼っていて、`make -j` だと CRD より先に Application が走る | 依存関係で書いた (`k8s-app` は `k8s-argocd` と `k8s-images` に依存)。`k8s-down` なども各ツールに依存させた |
| `k8s-lint` が kind まで go install していた (kubectl だけで足りる) | kubectl だけに依存させ、OS とアーキテクチャは `uname` で決める。CI の `k8s-ci` から Go のセットアップを外した |
| 検査が取得の失敗で途中終了する。枝の名前しか扱えない | 取得の失敗は空として NG に落とし、まとめまで出す。タグと SHA も比べられるようにした |
| `SECURE_COOKIE` が既定の true のまま、http のエッジで配っている | false を明示した。Secure の分岐は compose の `make https-verify` が測っている ([ADR 0023](0023-local-edge-and-https.md) 決定 3) |

## 実機で確かめたこと (2026-11-13)

`make k8s-verify` (`.github/scripts/k8s-probe.sh`) の結果。
Argo CD が `feat/k8s-gitops` から同期した状態で、全項目が通った。

**クラスタを消してゼロから一発で通した。** `make k8s-down` →
`make k8s-up K8S_REVISION=feat/k8s-gitops` が **191 秒で exit 0**
(イメージのビルドはキャッシュあり)、続く `make k8s-verify` も全項目 OK、
全 Pod の再起動は 0 回だった。それまでは手順を分けて叩いて確かめていたので、
**ひとつながりの `make k8s-up` はこれが初回**になる。

**レビューの指摘を直したあとも、もう一度ゼロから通した** (88f80e3):
`make k8s-up` が 185 秒で exit 0 (同期の待ちは 26 秒)、`make k8s-verify` 全項目 OK、
再起動 0 回。3 MiB の画像のアップロードも 201。その前に Argo CD なしの
`make k8s-apply-local` を、`db/init` の中身を変えて 2 回流し、2 回とも通った。

| 見たこと | 結果 |
| --- | --- |
| `/` → next-app、`/api/*` → go-api、`/images/*` → MinIO | 3 つとも届く (`/images/` は MinIO がバケット名つきで答える) |
| 正しい Origin の書き込み / 他所の Origin | 201 / 403 |
| マイグレーション | version 14 (ディレクトリの up 14 本と一致)、dirty=f |
| pg_trgm が日本語を索引できるか ([ADR 0012](0012-search.md)) | `show_trgm('検証')` が 3 個 (LC_CTYPE=C.UTF-8 が効いている) |
| Argo CD | Synced / Healthy、同期したコミットがブランチの先頭と一致 |
| go-api を 0 台に縮める | 2 秒で 1 台に戻った (3 回とも) |
| Service (mailpit) を消す | 6 秒 / 2 秒 / 3 秒で作り直された |
| Git の変更 (next-app に livenessProbe を足して push) | 揺らぎあり 123 秒 / 揺らぎ 0 で 51 秒で同期 |
| 3 MiB の画像のアップロード (DB に直接作ったセッションで) | 201。エッジ経由の取り出しも 200 |
| マニフェストの無い枝 (`main`) を指させる | 1 秒で NG (以前は 600 秒のタイムアウト) |
| postgres の Pod を消して作り直す (PVC の永続性) | スレッド 2・ユーザー 1・マイグレーション 14 のまま (前後で同じ)。readiness の変更で StatefulSet が作り直されたときも同じだった |

**検査の効きも確かめた** ([ADR 0022](0022-probing-the-checkers.md) と同じ考え方):

| 壊し方 | 検査の結果 |
| --- | --- |
| マイグレーションを 1 本足したディレクトリと比べさせる | 「最新でない (version 14, want 15)」で NG |
| Application の selfHeal を切って同じ崩し方をする | 3 件 NG。Argo CD は OutOfSync と表示するだけで戻さなかった。selfHeal を戻すと 4 秒で復旧 |
| kustomization.yaml からマイグレーションを 1 本消す | `make k8s-lint` が「足りない: 000014_...」で落ちる |

## まだ確かめていないこと

- **既定の枝 (`develop`) から同期したことが無い。** `develop` にはまだ `infra/k8s` が無い。
  マージ後に `make k8s-up` (K8S_REVISION なし) を通すまでは、
  application.yaml の既定のまま動くかは未確認
- **Google のログインを通していない** (OIDC 未設定)。アップロードは DB に
  直接作ったセッションで確かめたので、ログインの往復 (リダイレクトと Cookie の発行) は
  k8s 上では一度も通っていない
- **同期時間は 1 回ずつしか測っていない。**

## 引き受けるコスト

- **Argo CD の常駐分のメモリ。** 7 つの部品が常に動く。Docker に割り当てた
  8GB の中で compose と同時に立てると苦しい —— 両方を同時に動かす前提にしない
- **イメージの中身は Git に載らない** (決定 4)。GitOps と呼べるのはマニフェストまで
- **マイグレーション一覧の二重管理** (決定 3)。`make k8s-lint` で見張るが、
  一覧そのものは手で直す必要がある
- **MinIO のイメージの取得元が閉じる危険を、compose と共有する。**
  k8s も `bitnamilegacy` を使う ([ADR 0024](0024-aws-deployment.md) の 17)。
  `make k8s-images` はホストのキャッシュから読み込むので、手元に残っている間は
  動くが、キャッシュの無い PC では引けなくなる
- **compose と k8s の 2 系統を保つ。** 環境変数を足したら両方に足す。
  ここは検査で見張っていない —— go-api の設定を増やすときに見落としうる
  (覆す条件: 一度でも片方だけに足して壊したら、`config.go` の `os.Getenv` と
  マニフェストの env を突き合わせる検査を足す)

## やらないこと

- EKS への移行 (決定 7)
- Helm chart 化 (環境が 1 つしか無い)
- Argo CD 自身の GitOps 管理 (決定 2)
