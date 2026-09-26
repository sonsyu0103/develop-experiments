# ADR 0024: AWS へのデプロイ —— 「消せること」を構成の要件にする

- ステータス: **採用 (実装済み・実機検証済み)**。
  `infra/terraform` / `make tf-apply` / `.github/workflows/deploy.yml`
  (2026-08-25)。**実際に apply し、`make tf-verify` が全項目通ったうえで
  destroy まで一周した。** 残る未検証は下の「まだ確かめていないこと」を参照
- 日付: 2026-08-25

## 背景

[ADR 0023](0023-local-edge-and-https.md) で手元にエッジを置き、
プロキシ配下でしか通らない分岐を実測できるようにした。
[インフラ構成](../infrastructure.md) には理想の姿も描いてある。

**だが IaC は 1 行も無かった。**

構成図と Terraform は別物として見られる。
「本番に出すならこう置く」と書けることと、
**その構成を再現可能な形で持っていること**は、示せる能力が違う。

さらに、`terraform plan` が通るだけのコードは動く保証がない。
**一度も apply していない IaC は、書いていないのとあまり変わらない。**

## 決定 1: 常時公開せず、「立てて壊せること」を要件にする

月 $40 前後を払い続けるのではなく、**見せたい日に `apply` し、
終わったら `destroy` する。** 1 日あたり $1.3 ほど。

この運用を選ぶと、**「消せること」が構成の要件になる。**
AWS の既定は逆 (消えにくいほう) に倒れているので、明示的に外す:

| リソース | 既定 | この構成 | 外さないと何が起きるか |
| --- | --- | --- | --- |
| RDS | 最終スナップショットを取る | `skip_final_snapshot = true` | destroy が止まる |
| RDS | 削除保護あり | `deletion_protection = false` | destroy が止まる |
| S3 | 中身があると消せない | `force_destroy = true` | 画像を 1 枚投稿した時点で destroy が止まる |
| ECR | イメージがあると消せない | `force_delete = true` | push した時点で destroy が止まる |
| ALB | 削除保護あり | `enable_deletion_protection = false` | destroy が止まる |
| Secrets Manager | 30 日の復旧猶予 | `recovery_window_in_days = 0` | **同名で作り直せない** |

最後の 1 つが特に効く。既定のままだと destroy しても論理削除で残り、
次の apply が `already scheduled for deletion` で落ちる ——
**「消したのに作れない」**という、いちばん分かりにくい失敗になる。

**消し忘れは金額で見張る。** AWS Budgets で実績 80% と予測 100% に
通知を置く (`monitoring.tf`)。予算アラート自体は無料で、
この運用でいちばん怖い事故 (destroy を忘れて月末に請求が来る) に直接効く。

## 決定 2: Terraform を使う (CDK ではない)

参照した記事は AWS CDK for Go を使っている。
リポジトリが Go 一色なので言語は揃うが、**Terraform を採る。**

- 求人数と読み手の多さが違う。**ポートフォリオは読まれてこそ**
- `plan` の差分が読みやすい。CDK は CloudFormation の差分になるため、
  「何が変わるか」が一段遠い
- state の話ができる。CDK では CloudFormation が隠してしまう

CDK for Go を採る理由があるとすれば「言語の統一」だが、
**インフラを説明する相手は Go を読むとは限らない。**

## 決定 3: NAT Gateway を置かない

教科書どおりなら ECS タスクはプライベートサブネットに置き、
外向き通信 (ECR / Google OIDC / SES) を NAT に通す。

**だが NAT は起動しているだけで月 $33 前後**かかり、
この構成でいちばん高い部品になる —— ALB ($18) より高い。

ECS タスクを**パブリックサブネットに置き、パブリック IP を付けて
直接インターネットへ出す。** 受信は Security Group で ALB からのみに絞る。
**「パブリック IP が付いている」ことと「外から入れる」ことは別**になる。

RDS はプライベートサブネットに残す。RDS は外向き通信が要らないので、
NAT が無くても困らない。

本番なら NAT か VPC エンドポイントを置く。
理由は「タスクにパブリック IP を付けない」という運用ポリシーを
守りやすくするためで、**技術的な必要性ではない**。

## 決定 4: 独自ドメインを取らない

CloudFront の既定ドメイン (`*.cloudfront.net`) には**証明書が付いてくる**。
ドメイン代 (年 $13) と Route 53 のホストゾーン (月 $0.50) を払わずに
HTTPS で公開できる。

代償は 2 つ:

1. URL が覚えにくい (`d1234abcd.cloudfront.net`)
2. **CloudFront ←→ ALB 間が HTTP になる** ——
   ALB に証明書を付けるには独自ドメインが要るため

2 番目が技術的に効いてくる (下の「実装して分かったこと 2」)。

## 決定 5: CD は GitHub Actions + OIDC (CodePipeline ではない)

参照した記事は CodePipeline + CodeBuild + CodeDeploy を使っている。
**GitHub Actions を採る。**

- **CI が既に GitHub Actions にある。** 2 つの CI 基盤を持つ理由がない
- CodePipeline は**存在するだけで月 $1** かかる。
  「立てて壊す」運用と、常駐するパイプラインは相性が悪い
- CodeConnections の認可がコンソール作業になり、**IaC で完結しない**

**アクセスキーは置かない。** OIDC でロールを引き受ける (`iam.tf`)。
`sub` の条件でリポジトリを絞っており、ここを緩めると
GitHub 上の任意のリポジトリからロールを引き受けられる。

**push では動かさない。** この環境は立っていない日のほうが多いので、
push のたびに走らせると存在しないクラスタに向かって失敗し続ける。

## 決定 6: Blue/Green ではなくローリング

記事の中核は CodeDeploy による Blue/Green だが、採らない。

- Blue/Green には**ターゲットグループが 2 つ**要り、
  `appspec.yaml` と `taskdef.json` の管理が増える
- 切り戻しの速さが価値になるのは、**利用者が居るとき**。
  ここには居ない
- ECS のローリング更新でも、ヘルスチェックを通らないタスクは
  古いものと入れ替わらない —— **落ちないことは担保される**

「Blue/Green を知らない」のではなく、
**この構成では釣り合わない**という判断になる。
利用者が付いた日に CodeDeploy へ移す。

## 決定 7: 秘密は Secrets Manager 経由で渡す —— state に残すかは値ごとに分ける (2026-09-26 追記)

ECS のタスク定義は `ecs:DescribeTaskDefinition` があれば読める。
`environment` に入れた値はその応答に**平文で出る**ので、
秘密は `secrets` (`valueFrom`) で渡し、実行ロールに
Secrets Manager の読み取りを与える。

**当初 `DATABASE_URL` だけがこの形になっていて、
`GOOGLE_CLIENT_SECRET` は `environment` に平文で残っていた。**
「environment に書くと読める人全員に見える」というコメントの
3 行上にその行があった —— 意図は書けていて、適用漏れだけがあった形になる。

値ごとの置き場:

| 値 | 置き場 | 理由 |
| --- | --- | --- |
| `DATABASE_URL` | Secrets Manager (`bbs/db`) | 秘密 |
| `GOOGLE_CLIENT_SECRET` | Secrets Manager (`bbs/google-oauth`) | 秘密 |
| `GOOGLE_CLIENT_ID` | `environment` | **秘密ではない。** 認可のリダイレクト URL に載ってブラウザまで出る |

IAM は**実行ロール側**に要る。`secrets` を環境変数へ展開するのは
ECS エージェントの仕事なので、タスクロール (アプリ自身) に付けても
起動に失敗する。資源はワイルドカードにせず ARN を列挙する ——
`bbs/*` にすると、後で足した無関係な secret まで読めるようになる。

### 認証が無効なら secret ごと作らない

認証は任意 ([ADR 0005](0005-authentication.md))。`google_client_secret` が
空なら `count = 0` で secret を作らず、`secrets` への注入も行わない。

空文字のまま作ろうとしても Secrets Manager が受け付けない
(`SecretString` は 1 文字以上)。またアプリ側は `os.Getenv` で読むので
(`internal/config/config.go`)、**「環境変数が無い」と「空文字」は同じ挙動**に
なる —— `AuthConfig.Enabled()` が false を返し、ログインの 2 経路だけが 503。
`environment` から外しても、無効時の振る舞いは変わらない。

### `secret_string_wo` は google だけに使う。db には使わない

Terraform 1.11 の write-only 属性 (`*_wo`) を使うと、値が
**state にも plan ファイルにも残らない。** provider に apply 時だけ渡される。

代償がある。**値が state に無いので、Terraform は中身の変化を検知できない。**
更新は `secret_string_wo_version` を人が上げたときだけ起きる。

この代償を払えるかは、**secret の中身が何に依存しているか**で決まる:

| secret | 中身の出どころ | `_wo` | 理由 |
| --- | --- | --- | --- |
| `bbs/google-oauth` | `var.google_client_secret` 1 個 | **使う** | 他リソースに依存しない。ローテーション時に番号を上げるのは自然な操作 |
| `bbs/db` | `aws_db_instance.main` の `address` / `endpoint` | **使わない** | 追跡されないと、**RDS が置き換わっても secret が古いホストを指したまま**になる |

db 側のパスワードを state から消すなら、`_wo` ではなく
`manage_master_user_password = true` (AWS 側が secret を持つ) のほうが筋が通る。
`random_password` を `ephemeral` にする手も plan までは通ったが、
**生成した値を誰も保持しなくなる**ため、RDS の置き換えと secret の更新が
ずれたときに突き合わせる先が無くなる。採らない。

**`_wo` を使うと Terraform の下限が上がる。** `versions.tf` の
`required_version` を `~> 1.10` から `~> 1.11` に上げた。1.10 で apply すると
`Unsupported argument` で落ちるので、下限を据え置くと**通る構成が嘘になる。**

### `_wo` を実機で確かめた (2026-09-26)

ダミー値 (`google_client_id` は空のまま = ログインは無効) で apply し、
**plan では原理的に出ない部分**を実測した。

**1. 値は Secrets Manager に入り、state には残らない。**

`aws secretsmanager get-secret-value --secret-id bbs/google-oauth` が
`{"client_secret":"..."}` を返す一方、`terraform.tfstate` 内の
その文字列の出現数は **0**。同じ state の中で並べると差がはっきり出る:

| state 内の `secret_string` | |
| --- | --- |
| `secret_version.db` (素の `secret_string`) | **298 文字が平文で存在** |
| `secret_version.google_oauth` (`_wo`) | **null** |
| `random_password.result` (参考) | 32 文字が平文で存在 |

**2. `secrets` の 2 件目として注入される。**
`describe-task-definition` の実物で `DATABASE_URL` と
`GOOGLE_CLIENT_SECRET` の 2 件、`environment` 側に
`SECRET` を含む項目は 0 件。実行ロールのポリシーも ARN 2 本に増えていた。

**3. `wo_version` を上げ忘れると、plan が `No changes` を返す。**

ここがいちばん危ない。tfvars の値を書き換え、`wo_version` を 1 のまま
plan したところ:

```
No changes. Your infrastructure matches the configuration.
```

**「apply は成功するのに値が変わらない」よりさらに悪い。**
差分が 0 件なので、**apply する機会すら来ない。**
値が state に無いので比較対象が無く、Terraform には原理的に見えない。

`wo_version` を 2 に上げて apply すると更新され、Secrets Manager 側に
`AWSPREVIOUS` / `AWSCURRENT` の 2 版が並ぶことも確認した。

> **運用上の含み**: ローテーションを「tfvars を書き換えて apply」だけで
> 済ませると、**静かに失敗する。** 番号を上げる操作が手順から抜けたときに
> 気づく仕掛けが無い —— `wo_version` を値のハッシュから導出する
> (`secret_string_wo_version = parseint(substr(sha256(var.google_client_secret), 0, 8), 16)`)
> といった形にすれば自動で動くが、**まだ入れていない** (やり残し)。

### plan ファイルには秘密が入る —— `_wo` でも消えない

`terraform plan -out` が書くファイルは zip で、中身は
`tfconfig` / `tfplan` / `tfstate` / `tfstate-prev`。`_wo` にした値は
`tfstate` 側には入らないが、**`-var` で渡した値は `tfplan` に平文で残る**
(zip を展開して `tfplan` エントリに当てて確認した)。
変数そのものの性質で、`_wo` とは関係がない。

`make tf-apply` は plan を `$(HOME)/claude-artifacts/bbs-apply.tfplan` に
固めてから適用する (対話で止まらないようにするため)。
**成功すれば最後の `rm -f` で消える** (`tf-destroy` も同じ形)。

**危ないのは失敗したときで、`rm` の手前で make が止まるので残る。**
apply が途中で落ちるのは「作れたところまでは state に残る」場面
(下の 10) と重なるため、**いちばん取り乱しているときに、
秘密を含むファイルが成果物ディレクトリに置き去りになる。**
いまは `google_client_secret` を tfvars に入れていないので実害は無いが、
設定した日からは効いてくる (やり残しに追加)。

## コスト

東京リージョン、常時起動した場合の概算:

| リソース | 月額 | 備考 |
| --- | --- | --- |
| ALB | $18 | 固定費。いちばん高い |
| RDS `db.t4g.micro` (Single-AZ) | $15 | ストレージ 20 GiB 込み |
| Fargate Spot × 2 タスク | $5 | 0.25 vCPU / 0.5 GB。通常の Fargate なら $15 |
| CloudFront / S3 / ECR / Logs | ~$1 | ほぼ無料枠に収まる |
| **NAT Gateway** | **$0** | 置かない (決定 3) |
| **Route 53 + ドメイン** | **$0** | 取らない (決定 4) |
| 合計 | **約 $39/月** | **1 日あたり約 $1.3** |

## 実装して分かったこと (2026-08-25)

### 1. next-app に本番用のイメージが無かった

`apps/next-app/Dockerfile` は `npm run dev` を実行するだけの
1 ステージ構成だった。CI は `next build` を通していたが、
**コンテナ化は一度もされていない。**

multi-stage 化し (`base` → `dev` → `builder` → `prod`)、
`next.config.ts` に `output: 'standalone'` を足した。

**`HOSTNAME=0.0.0.0` が要る。** standalone の既定は localhost で、
これが無いとコンテナの外から届かない ——
ALB のヘルスチェックが延々と失敗し、
**タスクが起動と終了を繰り返す**形で現れる。

静的アセット (`.next/static`) は standalone に含まれないので
別途コピーする。忘れると CSS と JS が 404 になり、画面が崩れる。

手元でビルドして起動し、`/` が 200、静的アセットも 200 になることを確認した。

### 2. ALB は `X-Forwarded-Proto` を上書きする —— 手元の実測がそのまま効いた

CloudFront で HTTPS を終端し、ALB へ HTTP で流す構成 (決定 4) では、
**ALB が付ける `X-Forwarded-Proto` は `http` になる。**
ALB は「自分が受けたスキーム」で上書きするため、
CloudFront が何を受けたかは伝わらない。

go-api の `selfOrigin()` は `http://...` を組み立て、
`csrfGuard` は Origin (`https://...`) と一致せず、
**すべての書き込みを 403 にする。**

これは [ADR 0023](0023-local-edge-and-https.md) の 2 で
手元の Caddy を使って実測した壊れ方そのものになる。
そして解も同じ —— **`CORS_ALLOWED_ORIGINS` に公開ドメインを明示する。**
`isAllowedOrigin` は許可リストを先に見るので、`selfOrigin` まで到達しない。

**手元でエッジを立てていなければ、この構成は
「デプロイできたのに書き込みだけ全部 403」で詰まっていた。**

### 3. CloudFront のオリジンリクエストポリシーで `Host` を落としてはいけない

同じ理由で、`AllViewerExceptHostHeader` を選ぶと
ALB へ届く `Host` が ALB の DNS 名になる。
`selfOrigin()` は `Host` からオリジンを組み立てるので、
**許可リストとも一致しなくなり、逃げ道が無くなる。**

`AllViewer` を使う。

### 4. ALB はパスを書き換えられない

手元の Caddy は `handle_path /api/*` でプレフィックスを剥がしていた。
**ALB のリスナールールに書き換えは無い** (リダイレクトはできるが、
API のリクエストとしては成立しない)。

取れる形は 3 つあった:

1. go-api 側で `/api` を受ける → OpenAPI の servers まで影響が及ぶ
2. CloudFront にオリジンを 2 つ置き、カスタムヘッダで ALB 側を振り分ける
3. **ALB のリスナーをポートで分ける** ← 採用

80 を next-app、8080 を go-api に割り当て、CloudFront 側で
`/api/*` を 8080 のオリジンへ向け、**CloudFront Function で剥がす。**
剥がす場所が Caddy から CloudFront へ移るだけで、go-api から見た形は変わらない。
リスナーの追加に料金はかからない。

### 5. distroless には ECS Exec で入れない

`enable_execute_command` を有効にしても、go-api の prod イメージは
distroless で **shell を持たない**ので入れない。
「踏み台を立てずに psql する」という当初の想定は成立しなかった。

有効のままにしてあるのは、next-app 側 (alpine) には効くのと、
障害時に debug イメージへ差し替えれば使えるため。
`make tf-secret` は接続情報を表示するだけに留めた。

### 6. ARM64 で焼かないと `exec format error` になる

タスク定義で Graviton (ARM64) を指定しているので、
イメージも ARM64 で焼く必要がある。x86 のイメージを push すると
タスクは起動を試みて落ち続けるが、**ECS のイベントには
「タスクが停止した」としか出ない。**

`infra/scripts/push-images.sh` は `--platform linux/arm64` を明示し、
CD は ARM ランナー (`ubuntu-24.04-arm`) を使う。
手元で 3 つとも ARM64 でビルドし、起動することを確認した
(go-api は `/healthz` `/readyz` とも 200、migrate は `no change` で exit 0)。

### 7. `terraform output` は state が無くても exit 0 を返す

`apply` する前に `make tf-push` を叩いたとき、
**terraform のエラーと python の traceback が並んで出て、
本当の原因 (apply していない) が読み取れなかった。**

デプロイ用スクリプトに「環境が立っているか」の確認を入れたが、
最初の実装は効かなかった:

```bash
if ! tf output -raw ecs_cluster_name >/dev/null 2>&1; then   # 素通りする
```

`terraform output` は state が無い場合、**標準エラーにメッセージを出しつつ
終了コード 0 を返す。** `-json` なら `{}` が返るので、そちらで判定する。

**CD (`deploy.yml`) には同じ確認を最初から入れていた** のに、
手元のスクリプトには無かった。
検査の穴は「片方だけ直す」で生まれる ——
[ADR 0023](0023-local-edge-and-https.md) の 6 と同じ形になる。

### 8. Security Group の description に日本語を入れると apply が落ちる

**`terraform validate` は通り、`plan` も通り、`apply` の途中で 3 つ同時に失敗した。**

```
InvalidParameterValue: Value (ALB。CloudFront からの HTTP のみ受ける)
for parameter GroupDescription is invalid.
Character sets beyond ASCII are not supported.
```

EC2 API は Security Group とそのルールの description に
**ASCII しか受け付けない。** このリポジトリはコメントを日本語で
厚く書く方針なので、そのまま `description` にも日本語を入れていた。

**AWS の中でも制約が揃っていない** ——
CloudFront の `comment` と Budgets の名前は日本語のまま作成できた。
古い API (EC2) にだけ残っている制限になる。

説明はコメント側に日本語で残し、**API に送る値だけ英語**にした。

**これは「validate で落とせないもの」の実例になる。**
上の CI の節に「IAM ポリシーが実際に足りるか、SG が本当に通るかは
apply するまで分からない」と書いたとおりのことが起きた ——
書いた本人が最初に踏んだ。

このとき ALB / RDS / CloudFront / ECS サービスは**まだ作られていない**ので、
課金が発生していたのは Secrets Manager (月 $0.40) だけだった。
**依存の順序が、失敗したときの損害を決めている。**

### 9. マネージドプレフィックスリストは、CIDR の数だけルール枠を消費する

8 を直して apply し直すと、次はこれで落ちた:

```
RulesPerSecurityGroupLimitExceeded:
The maximum number of rules per security group has been reached.
```

ALB の SG に書いたルールは 4 つだけ。上限は 60。**一見あり得ない。**

原因は `data.aws_ec2_managed_prefix_list.cloudfront_origin_facing` で、
**プレフィックスリストは含まれる CIDR の数だけルール枠を消費する。**

| | 値 |
| --- | --- |
| CloudFront のプレフィックスリストの CIDR 数 | 46 |
| SG あたりのルール上限 (既定) | 60 |
| 80 番と 8080 番の 2 箇所で使うと | 46 x 2 + 2 = **94** |

**1 行に見えて 46 行分**になる。

対処は 2 つあった:

1. クォータを 60 -> 120 に引き上げ申請する (`Adjustable: true`)
2. **ALB の SG を用途ごとに 2 つに分ける** ← 採用

ALB は SG を複数アタッチでき、評価は**すべての SG の和集合**になる。
`alb_web` (47) と `alb_api` (48) に分ければ、それぞれ 60 に収まる。

**申請の承認を待たずに済むほうを採った。** 用途で分かれるぶん読みやすくもなる。

### 10. 失敗しても、作れたところまでは state に残る

8 と 9 で 2 回失敗したが、そのたびに全部やり直しにはならなかった。
1 回目は 54 リソース、2 回目はそこに ALB / RDS / CloudFront が加わった
状態で state に記録され、次の plan は**差分だけ**になる。

これは destroy でも同じで、**部分的に作られた状態も destroy できる。**
決定 1 の「消せること」は、成功したときだけでなく
**失敗したときにこそ効く**要件になる。

### 11. Terraform は Security Group の「差し替え順序」を保証しない

9 の対処 (SG を 2 つに分ける) を apply したところ、
**古い SG の削除で 11 分待ち、タイムアウトした。**

```
DependencyViolation: resource sg-028d6eab... has a dependent object
```

plan は正しく 3 つを予定していた:

```
aws_lb.main                        will be updated in-place  (SG の付け替え)
aws_vpc_security_group_ingress_rule.ecs_*_from_alb  will be updated in-place
aws_security_group.alb             will be destroyed
```

**だが順序は保証されない。** Terraform は依存グラフ上で
「古い SG はもう誰も参照していない」と判断して削除を先に投げたが、
**AWS の実態では ALB と ECS の SG ルールがまだ掴んでいた。**

構成としての正しさ (誰が誰を参照すべきか) と、
**適用の途中で通る状態**は別物になる。

手で解いた順序はこうなる:

1. `elbv2 set-security-groups` で ALB を新しい 2 つに付け替える
2. `ec2 modify-security-group-rules` で ECS 側の参照先を新しい SG に変える
3. 古い SG を削除する
4. `terraform plan` で state と実態を突き合わせ、残りを apply する

**Terraform は「あるべき状態」を書く道具であって、
「そこへ至る途中の状態」までは書けない。** IaC を使っていても、
AWS 側の反映ラグと依存の解消順序は自分で面倒を見ることになる。

同じ形を避けるなら、SG の入れ替えは
**「新しい SG を足す」「参照を移す」「古い SG を消す」を
別々の apply に分ける**のが安全になる。

### 12. VPC の中から internet-facing ALB を引くと、パブリック IP が返る

**予告していた壊れ方が、予告と違う原因で起きた。**

`network.tf` にはこう書いてあった:

> ここを開けないと **画面だけが真っ白になる** —— API は正常、
> ALB も正常、Server Components からの取得だけが落ちる。

実際そうなった。API (`/api/*`) は健全、RDS まで到達、書き込みも通る。
**`/` だけが届かない。** next-app のログにはこう出ていた:

```
Connect Timeout Error (attempted addresses: 54.238.198.29:8080, ...)
```

`54.238.198.29` は **ALB のパブリック IP**。つまり:

1. internet-facing な ALB の DNS 名は、**VPC の中から引いてもパブリック IP を返す**
2. ECS タスクはパブリック IP を持つので (決定 3 で NAT を置かないため)、
   そこへの通信は**いったんインターネットに出て戻る**
3. ALB から見た送信元は「ECS のパブリック IP」であって、**ECS の Security Group ではない**
4. `referenced_security_group_id` による許可は **VPC 内の通信にしか効かない** → 拒否

**SG の書き方は正しかった。経路がそもそも VPC の外を通っていた。**

「SG を 1 本開け忘れると画面が真っ白になる」という予測は当たったが、
**開け忘れたのではなく、開けても効かない経路だった**ことになる。
決定 3 (NAT を置かない = タスクにパブリック IP を付ける) の副作用が、
ここで初めて表に出た。

当面の対処は `API_URL` を CloudFront 経由にすること。
往復は増えるが、Server Components からの取得は GET だけなので
csrfGuard にも当たらない ([ADR 0013](0013-http-defense.md) の 7)。

**本来は ECS Service Connect を使う場面になる** (下の「やり残し」)。

### 13. `terraform destroy` は対話を求める —— 端末が無いと「消したつもり」になる

`make tf-destroy` を端末の無い環境から実行したところ、
**確認プロンプトで止まり、1 つも消えなかった。**
出力には消える予定のリソースがずらりと並ぶので、
**一見すると消えたように見える。**

これは決定 1 (使う日だけ立てる) と正面から衝突する。
消し忘れがいちばん怖い運用なのに、
**「消したつもりで課金が続く」経路が既定で存在していた。**

`plan -destroy` でファイルに固めてから `apply` する形に変えた。
何が消えるかは plan の出力で見えるし、適用は非対話で完了する。

**apply でも同じ形を踏んでいる** (plan ファイルを渡す方式にした)。
`terraform` の対話は「人間が端末の前にいる」ことを前提にしており、
**自動化の経路では必ずこれを外す**必要がある。

### 14. 「動いた」の裏で壊れていた 2 つ —— 検査が通ることと正しいことは違う

`make tf-verify` が全項目を通り、画面も表示された。
**それでも 2 つ壊れていた** (レビューで発覚):

**`NEXT_PUBLIC_API_URL` がクライアントバンドルに焼かれていなかった。**
`NEXT_PUBLIC_*` は `next build` の時点で定数に置換されるので、
ECS の環境変数として渡しても効かない。**`app/lib/api.ts` には
「本番のイメージを作る段になったら、ビルド引数として渡すこと」と
書いてあった** —— 本番イメージを新設したときに、その注意書きを踏んだ。

結果、ブラウザ側の呼び出し (フォーム送信、ログインリンク、管理画面) が
すべて **利用者自身の `localhost:8080`** を叩く。
`tf-verify` は `/api/*` を直接 curl するので**この経路を通らない**。

対処は相対パス (`/api`) をビルド引数の既定にすること。
エッジの背後では画面と API が同一オリジンなので、
**ホスト名を焼き込む必要がそもそも無い** ——
CloudFront のドメインが apply するまで決まらない循環も同時に消える。

**`S3_PUBLIC_BASE_URL` が `images/` を二重に付けていた。**
オブジェクトキーが `images/<uuid>.webp` で始まるので、
`https://<cf>/images` を前置きすると `/images/images/<uuid>.webp` になる。
手元の Caddy は `handle_path` がプレフィックスを剥がしてから
`/bbs-images` を足すので**同じ値で成立していた** ——
その値をそのまま持ってきたのが原因になる。

**どちらも「検査が通った」状態で潜んでいた。**
`tf-verify` の画像検査は 200/403/404 をまとめて成功にしていたので、
403 を返していても通ってしまった。**検査の甘さが不具合を隠した**
形になり、[ADR 0022](0022-probing-the-checkers.md) の
「検査そのものを検査する」がここでも要るという話になる。

### 15. 「直したつもり」が 1 段浅かった —— XFF は経路の段数だけ深くなる

[ADR 0023](0023-local-edge-and-https.md) で、`TRUSTED_PROXIES` を設定すると
`ClientIP` が Caddy の IP (`172.23.0.8`) から実際の送信元 (`172.23.0.1`) に
変わることを実測した。同じ考えで AWS 側にも VPC CIDR を渡していた。

**足りなかった。** 手元は Caddy が 1 段だけの構成だったが、
AWS は CloudFront と ALB で 2 段になる。XFF はこう届く:

```
X-Forwarded-For: viewer-ip, cloudfront-edge-ip
                             ^ ALB が直前ピア (エッジ) を追記する
```

gin の `ClientIP` は XFF を**右から辿り、最初の「信頼していない IP」を返す**。
VPC CIDR だけを信頼すると、右端のエッジ IP がそのまま `ClientIP` になる。

手元で対照実験して確かめた (compose のネットワークを VPC に見立て、
`13.32.0.0/16` をエッジに見立てる):

| `TRUSTED_PROXIES` | `client_ip` |
| --- | --- |
| VPC のみ | `13.32.0.5` (エッジ) |
| VPC + エッジ | `203.0.113.9` (閲覧者) |

**ALB の IP に潰れる問題を直したつもりが、エッジの IP に潰れる形で
残っていた。** 問い合わせのレート制限は同じ PoP を通る利用者どうしで
共有され、閲覧数の重複抑制 ([ADR 0006](0006-view-count-and-popularity.md) の
`visitorKey`) も同じ値を使うので、同じ PoP からの閲覧が 1 人分に丸められる。

対処は、SG で使っているのと同じプレフィックスリストから CIDR を取り、
信頼範囲に含めること (`locals.tf`)。47 個の CIDR (677 文字) を
環境変数で渡しても gin が問題なく受けることは実測した。

**教訓は「実測した結論にも射程がある」ということになる。**
手元の 1 段構成で得た答えは、2 段構成では成り立たなかった。

### 16. 検査 6 番は、書いてから一度も実行されていなかった (2026-09-26)

`make tf-verify` の「存在しないキーは 404」を見る検査が **NG** で落ちた。
だが**インフラではなく検査側が間違っていた。**

S3 は `s3:ListBucket` を持たない主体に対して、
**オブジェクトが存在しないときも 403 `AccessDenied` を返す**
(「無い」ことを教えないため)。バケットポリシーは `s3:GetObject` だけを
与えていて、`ListBucket` はリポジトリのどこにも無かった ——
つまり **404 は原理的に返らない。** 実測の応答もそれだった:

```
HTTP/2 403      server: AmazonS3
<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>
```

検査のコメントは「バケットポリシーが `arn/*` に GetObject を
与えているため 404 が返る」と書いてあったが、**理屈が逆**になる。
404 を返させるには `ListBucket` が要る。

**なぜ今まで気づかなかったか:**

| | 日付 |
| --- | --- |
| `apply` → `verify` → `destroy` を一周した | 2026-08-25 |
| この検査を追加した (`7e88be2`) | **2026-08-26** |

**一周した翌日に足されている。** レビュー指摘への対応として
「検査を厚くする」変更を入れたが、その検査自体は AWS に当てていない。
上の「まだ確かめていないこと」に書いてあった監視 3 件と同じ形の穴で、
**検査を足す変更は、足した検査が未検証になる**という構造がある
([ADR 0022](0022-probing-the-checkers.md) の主題そのもの)。

**対処はポリシー側を直した。** 検査の期待値を 403 に合わせる手もあったが、
**403 は「OAC が壊れているとき」にも返る**ので、それでは何も証明しない。
`ListBucket` を (ディストリビューション条件付きで) 与えると:

- 存在しないキーが **404** になる —— 消えた画像と設定ミスが区別できる
- 404 が返ること自体が「**認可が成立している**」証明になり、検査が意味を持つ

リソースはバケット本体に書く (`arn/*` ではない)。ListBucket は
バケットに対する操作なので、`/*` を付けると効かない。

**教訓: 検査が落ちたとき、まず疑うのは検査のほう。**
今回は「インフラが壊れている」ではなく「検査が一度も動いていなかった」だった。

## 本番ならこうする

**この構成はポートフォリオ用であり、本番構成ではない。**
どこを変えるかを明示しておく。

| 項目 | この構成 | 本番 |
| --- | --- | --- |
| ECS の配置 | パブリックサブネット + パブリック IP | プライベート + NAT / VPC エンドポイント |
| ドメイン | CloudFront 既定 | 独自ドメイン + ACM |
| CloudFront ↔ ALB | HTTP | HTTPS (`X-Forwarded-Proto` も正しくなる) |
| RDS | Single-AZ / バックアップ無し | Multi-AZ / PITR |
| 削除保護 | すべて無効 | すべて有効 |
| Fargate | Spot のみ | On-Demand + Spot の混在 |
| ログ | CloudWatch (7 日) | FireLens → S3 → Athena ([ADR 0010](0010-log-pipeline.md)) |
| デプロイ | ローリング | Blue/Green (CodeDeploy) |
| tfstate | local | S3 backend (`use_lockfile`) |
| DB のパスワード | `random_password` (state に平文) | `manage_master_user_password` + 自動ローテーション (決定 7) |
| WAF | 無し | あり ([インフラ構成](../infrastructure.md) の図) |

## 実機で確かめたこと (2026-08-25)

`apply` → `make tf-push` → `make tf-migrate` → `make tf-verify` → `destroy` を
一周した。公開 URL は CloudFront の既定ドメイン。

初版で「怪しい」と挙げた 5 点の結果:

| 疑っていたこと | 結果 |
| --- | --- |
| CloudFront のプレフィックスリストで ALB が受けられるか | **受けられた。** 直接叩くと届かないことも確認 |
| `CORS_ALLOWED_ORIGINS` の明示で書き込みが通るか | **通った。** 手元の実測 (ADR 0023) がそのまま効いた |
| CloudFront Function のパス剥がしが期待どおりか | **期待どおり。** `/api/threads` が `/threads` として届く |
| Server Components から ALB に届くか | **届かなかった。** 下の 12 を参照 |
| マイグレーションのタスクが ECR に届くか | **届いた。** 拡張の作成漏れは別問題 (5) |

**外れたのは 4 番目だけ**で、しかも「SG を開け忘れる」という予測とは
違う原因 (経路が VPC の外を通っていた) だった。

`destroy` 後、ALB / RDS / CloudFront / NAT がいずれも 0 件であることと、
タグ検索に残るのが `INACTIVE` の墓標だけであることを確認した。
実績コストは約 $0.07 (稼働 1 時間)。

> **2026-09-26 の一周で分かったこと: タグ検索は 0 件にならない。**
> `get-resources --tag-filters Key=Project,Values=bbs` が **19 件**返した。
> 内訳は ECS の `INACTIVE` な墓標 (クラスタ・サービス・タスク定義 7 版) と、
> **すでに存在しない Security Group ルールの ARN 8 件** ——
> `describe-security-group-rules` に当てると
> `InvalidSecurityGroupRuleId.NotFound` で、タグ索引側が遅れているだけだった。
>
> つまり **「タグ検索が 0 件なら消えている」という読み方は成立しない。**
> 件数ではなく**状態**を見る必要がある:
>
> - ECS: `describe-clusters` の `status` が `INACTIVE` か
>   (`list-clusters` は削除済みを返さないので 0 件が正しい姿)
> - 課金するもの: `describe-db-instances` / `describe-load-balancers` /
>   `describe-nat-gateways` / `list-distributions` /
>   `describe-vpcs --filter isDefault=false` / `list-secrets` を直接引く
>
> `destroy` 後に 78 リソースすべてが消え、上記がいずれも 0 件になることを
> 確認した (`Apply complete! Resources: 0 added, 0 changed, 78 destroyed.`
> —— destroy plan を適用する形なので、出力は "Destroy complete" ではなく
> **"Apply complete"** になる。`grep` で確認するときに引っかかる)。

### 作られるリソースは 75 件 (2026-08-30 に内訳を確定、2026-09-17 に 71 → 74、2026-09-26 に 74 → 75 を訂正)

**`.tf` を `grep` しても 70 件しか出ない。** `count` と `for_each` で
展開される分が数えられないため。内訳を残しておく。

| | 件数 |
| --- | --- |
| `resource` ブロック (静的) | 70 |
| うち固定 (展開なし) | 60 |
| ECR: `for_each = local.ecr_repositories` (api / web / migrate) × 2 ブロック | **6** |
| network: `count = var.az_count` (既定 2) × 4 ブロック | **8** |
| IAM: GitHub OIDC プロバイダ (三項で 1 か 0 のどちらか) | **1** |
| monitoring: SNS 購読 (`var.alarm_email` 既定 `""` なので 0) | **0** |
| secrets: google の secret + version (`google_client_secret` 既定 `""` なので 0) | **0** |
| **合計** | **75** |

60 + 6 + 8 + 1 + 0 + 0 = 75。**`alarm_email` を設定して apply すると 76、
さらに `google_client_secret` も設定すると 78 になる。**

> **2026-09-26 に 74 → 75 を訂正した。** 決定 7 の作業中に
> `terraform plan` を実測したら 76 件 (`alarm_email` 設定済み) で、
> この表から出る 75 と **1 件合わなかった。**
>
> 原因はこの表の算数で、**構成は 2026-09-17 から変わっていない**
> (`git log -- infra/terraform` で確認)。展開されるブロックは
> ECR 2 + network 4 + OIDC 1 + SNS 1 = **8 ブロック**なので、
> 固定は 68 − 8 = **60**。ここが 59 になっていた。
>
> つまり **2026-09-17 の「71 → 74」も、正しくは「72 → 75」だった。**
> 内訳を書いておいたおかげで、実測とぶつけた時点で場所が特定できた。
>
> **同日 apply して裏を取った**: `Apply complete! Resources: 76 added`
> (`alarm_email` 設定済み・`google_client_secret` 未設定)。
> 表から出る 75 + SNS 購読 1 = 76 で一致する。
>
> 静的ブロックが 68 → 70 になっているのは、決定 7 で `secrets.tf` に
> 2 ブロック足したため。どちらも `count` 付きで、既定では 0 件に畳まれる。

> **2026-09-17 に監視を 3 リソース足した** (`monitoring.tf`: `target_5xx` / `api_error` のメトリクスフィルタ / `api_error_log`)。
> アプリが返した 5xx と ERROR ログを見るアラームが無かったため
> ([インフラ構成](../infrastructure.md) の「アラームを鳴らすもの」)。
> **apply → verify → destroy を一周したのは 71 件の時点**で、この 3 件は `terraform validate` までしか通していない。

> **なぜ数え直したか**: 実測で語る流儀で来ているのに、この数字だけ
> 出典が `apply` 時の記憶しか無かった。リポジトリを見た人が `grep` すると
> 65 になり、**説明できない差が 6 件残る**。ここに書いたので追える。

## まだ確かめていないこと

- **CD (`.github/workflows/deploy.yml`) を一度も実行していない。**
  手元のスクリプトは開発者の管理者権限で動くので、
  **OIDC ロールの権限不足はローカルでは露見しない** (レビューで
  `ecs:DescribeClusters` などの欠落が見つかった)。
  次に立てたら、まず `gh workflow run deploy.yml` を通すこと
- **Google OIDC を設定していない。** `tf-verify` の Cookie 検査が SKIP のまま
- **予算アラートのメールが届くか。** SNS の購読確認を押していない
- **2026-09-17 に足した監視 3 件が実際に鳴るか。** `terraform validate` まで。
  メトリクスフィルタのパターンがログに一致することは、ログの形をテスト
  (`TestNewHandler_ErrorLevelMatchesMetricFilter`) で固定しただけで、AWS 上では確かめていない
- **決定 7 のうち、google 側 (`secret_string_wo`) を apply していない。**

  `container_definitions` は **plan では unknown** になる ——
  新しい secret の ARN が apply 後にしか決まらないため、Terraform が
  丸ごと「known after apply」に倒す。IAM のポリシー JSON も同じ。
  そこで 2026-09-26 に apply し、`describe-task-definition` で実物を見た:

  | 見たもの | 結果 |
  | --- | --- |
  | `environment` に秘密が残っていないか | **残っていない。** `GOOGLE_CLIENT_ID` だけ (値は空) |
  | `secrets` の注入 | `DATABASE_URL` → `bbs/db:url::` |
  | 実行ロールのポリシー | `read-secrets` が `bbs/db` の ARN 1 本だけを許可 |

  続けて `google_client_secret` にダミーを入れて apply し、
  `count = 1` 側も通した。**結果は下の「`_wo` を実機で確かめた」を参照** ——
  未検証だった 3 点はすべて潰れている

## やり残し

- **ECS Service Connect による内部通信。**
  12 の対処は「CloudFront を経由して自分に戻る」形で、往復が増えている。
  Service Connect を入れると go-api を `http://go-api:8080` の名前で引けるようになり、
  **手元の compose と同じ形に揃う。** 追加料金もかからない
- **`make tf-verify` を CI に組み込む。** いまは手で回している
- **Google OIDC の設定。** `tf-verify` の Cookie 検査が SKIP のままになっている
- **`secret_string_wo_version` を手で上げる形をやめる。** 上げ忘れると
  plan が `No changes` を返し、**静かに失敗する** (決定 7 で実測)。
  値のハッシュから導出すれば自動で動く:
  `parseint(substr(sha256(var.google_client_secret), 0, 8), 16)`。
  ただし**番号が単調増加しない**組み合わせがありうるので、
  provider がそれを許すかを先に確かめる
- **`make tf-verify` の検査自体を一度も AWS に当てずに増やさない (16)。**
  検査を足す変更は、足した検査が未検証のまま残る。
  CI に組み込むか、検査を足した日に apply して当てる運用にする
- **plan ファイルの後始末を失敗経路にも付ける (決定 7 の最後)。**
  `make tf-apply` / `tf-destroy` は成功時に `rm -f` するが、
  **apply が落ちると `rm` に到達せず、秘密を含む plan が残る。**
  `trap` なり `|| (rm -f ...; false)` なりで、失敗しても消えるようにする
- **db secret の `manage_master_user_password` 化。** 決定 7 のとおり
  `_wo` では解けない。state から DB のパスワードを消すなら、
  AWS 側に secret を持たせる形になる。ただし secret 名が AWS 任せになるため、
  `recovery_window_in_days = 0` で回している「立てて壊す」運用と
  ぶつからないかを先に確かめる必要がある

## やらないこと

- **常時公開。** 決定 1 のとおり
- **FireLens によるログ基盤。** ADR 0010 の構成は S3 に貯まってから
  意味を持つが、立てて壊す環境では貯まる前に消える
- **WAF。** 月 $5 + リクエスト課金。利用者が居ない環境で払う理由がない
- **マルチ環境 (dev / stg / prod)。** 1 つで足りる。
  必要になったら workspace か tfvars で分ける
