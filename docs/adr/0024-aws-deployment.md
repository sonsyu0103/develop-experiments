# ADR 0024: AWS へのデプロイ —— 「消せること」を構成の要件にする

- ステータス: **採用 (コードは完成、`apply` は未実施)**。
  `infra/terraform` / `make tf-apply` / `.github/workflows/deploy.yml`
  (2026-08-25)。**実際に AWS 上で動かした検証はまだ無い** ——
  下の「まだ確かめていないこと」を参照
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
| WAF | 無し | あり ([インフラ構成](../infrastructure.md) の図) |

## まだ確かめていないこと

**`terraform apply` を一度も実行していない。**
`validate` と `fmt` は通り、3 つのイメージは手元でビルドして
起動まで確認したが、**AWS 上で組み上がることは未検証**になる。

とくに次が怪しい。実際に立てたら、まずここを疑う:

1. **CloudFront のマネージドプレフィックスリスト**
   (`com.amazonaws.global.cloudfront.origin-facing`) で
   ALB が本当に受けられるか。塞ぎすぎると 504 になる
2. **決定 2 の回避策が効くか。** `CORS_ALLOWED_ORIGINS` の明示で
   書き込みが通ることを、実際の CloudFront ドメインで確認する
3. **CloudFront Function のパス剥がしが期待どおりか。**
   `/api/threads` が go-api に `/threads` として届くか
4. **next-app の Server Components から ALB:8080 に届くか。**
   SG を 1 本開け忘れると、API は正常なのに画面だけ真っ白になる
5. **マイグレーションのタスクが VPC 内から ECR に届くか。**
   NAT が無いので、パブリック IP が付いていることが前提になる

これらは [ADR 0023](0023-local-edge-and-https.md) と同じ考え方で、
**apply したあとに `make tf-verify` で実測する** (`infra/scripts/verify.sh`)。
検査は先に書いてある —— 立てた瞬間に、上の 5 つがそのまま検査項目として走る。

**ALB を直接叩いて届かないことも見る。** 迂回できると、
CloudFront で付けているセキュリティヘッダが素通しになるため。

## やり残し

- **ECS Service Connect による内部通信。**
  12 の対処は「CloudFront を経由して自分に戻る」形で、往復が増えている。
  Service Connect を入れると go-api を `http://go-api:8080` の名前で引けるようになり、
  **手元の compose と同じ形に揃う。** 追加料金もかからない
- **`make tf-verify` を CI に組み込む。** いまは手で回している
- **Google OIDC の設定。** `tf-verify` の Cookie 検査が SKIP のままになっている

## やらないこと

- **常時公開。** 決定 1 のとおり
- **FireLens によるログ基盤。** ADR 0010 の構成は S3 に貯まってから
  意味を持つが、立てて壊す環境では貯まる前に消える
- **WAF。** 月 $5 + リクエスト課金。利用者が居ない環境で払う理由がない
- **マルチ環境 (dev / stg / prod)。** 1 つで足りる。
  必要になったら workspace か tfvars で分ける
