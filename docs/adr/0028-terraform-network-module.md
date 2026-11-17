# ADR 0028: network.tf の VPC・サブネット・ルーティングを module に切り出し、`moved` で state を移す —— Security Group は呼び出し側に残す

- ステータス: **採用 (実装済み・mock の state で確認済み)**。
  `infra/terraform/modules/network` / `make tf-test` (2026-11-17)。
  PR #76 のレビューで 7 件の指摘を受けた (「レビューで見つかったこと」)。
  **実際の AWS の state に対しては plan していない** (「まだ確かめていないこと」)
- 日付: 2026-11-17

## 背景

`infra/terraform` は 14 ファイルのフラットな構成で、`module` ブロックも
`moved` / `removed` ブロックも 1 件も無かった ([ADR 0024](0024-aws-deployment.md))。
Terraform で手を動かしていないのは **module の分割と state のアドレス操作**の 2 つで、
どちらも「すでに apply された環境を壊さずに構成を組み替える」ときに要る。

`network.tf` (280 行) には 2 種類のものが同居していた:

| 中身 | 資源 | アプリを知っているか |
| --- | --- | --- |
| VPC・IGW・サブネット・ルートテーブル | 8 ブロック (`count` 展開後 12 件) | 知らない。CIDR と AZ が決まれば決まる |
| Security Group とそのルール | 12 ブロック + prefix list の data 1 つ | 知っている。ALB → ECS → RDS の経路と、80 / 8080 / 3000 / 5432 の番号 |

state は destroy 後で空 (資源 0 件)。AWS の認証は手元に無い (`aws sts get-caller-identity` が
`NoCredentials`)。

## 検討した選択肢

### 切り出す範囲

#### A. `network.tf` を丸ごと module にする

- 利点: 1 ファイル = 1 module で分かりやすい。外から見える出力も SG の ID だけで済む
- 欠点: module が「ALB は 80 と 8080、ECS は 3000 と 8080 を受ける」を知ることになる。
  別のアプリに使えず、アプリの経路を 1 本足すたびに module の入出力が変わる

#### B. VPC・サブネット・ルーティングだけを module にし、SG は呼び出し側に残す (採用)

- 利点: module はアプリを知らない。入力は名前・CIDR・AZ の 3 つ、出力は VPC とサブネットの ID の 3 つ
- 利点: SG の「誰が誰に話せるか」が、それを使う ALB・ECS・RDS と同じ層に残る
- 欠点: 「ネットワークまわり」が module と `network.tf` の 2 か所に分かれる

#### C. 公開 module (`terraform-aws-modules/vpc/aws`) に乗り換える

- 利点: NAT・VPC エンドポイント・フローログなどが変数 1 つで足せる
- 欠点: 中の資源のアドレスが変わるので `moved` が 12 件では済まず、
  module の版を上げるたびに state のアドレスが動きうる。
  NAT を置かない ([ADR 0024](0024-aws-deployment.md)) この構成では、使う機能が少ない
- 欠点: 今回の目的 (module と `moved` を自分で書く) が外注になる

### 確かめ方

#### A. `make tf-plan` で No changes を見る

- 欠点: **state が空なので、前後とも「75 件作る」になり、`moved` が効いたかは見えない。**
  認証も無い。見るために apply すると課金が発生する

#### B. `terraform test` の `mock_provider` で、旧構成の state を作ってから新構成で plan する (採用)

- 利点: 認証も課金も要らない。data source も mock が値を返す
- 利点: `state_key` を揃えると、別の構成 (`module { source = "../before" }`) で作った state に
  対して、今の構成で plan できる —— 「apply 済みの環境に、組み替えた構成を当てる」を再現できる
- 欠点: mock の値は本物ではない (「まだ確かめていないこと」)

## 決定

選択肢 B で切り出し、8 ブロックに `moved` を 1 つずつ書いた。決め手は、
**module の入出力がアプリの経路の変更で動かないこと。** SG まで入れると、
番号を 1 つ変えるたびに module を触ることになり、分けた意味が薄い。

- module は `availability_zones` を**名前のリストで**受け取る (AZ の数ではなく)。
  data source を module の中で引くと、どの AZ になるかが外から見えず、テストで差し替えられない。
  AZ を何個使うか (`az_count >= 2`、ALB の制約) は呼び出し側の関心なので、検証も呼び出し側に残した。
  module 側の下限は 1、上限は 10 (CIDR の切り方の都合。罠 4)
- `count` 付きの資源は、資源ごと `moved` を書けば `[0]` `[1]` もまとめて移る

確かめた結果 (scratch に旧構成を写して実行。リポジトリには入れていない。手順は「moved の確かめ方」):

| 構成 | plan |
| --- | --- |
| module + `moved` | 12 件すべて `has moved to module.network...`。**`Plan: 0 to add, 0 to change, 0 to destroy.`** |
| module のみ (`moved` を外した) | **`Plan: 12 to add, 10 to change, 12 to destroy.`** VPC ごと作り直し、ALB・ECS サービス・DB サブネットグループなど 10 件が巻き込まれる |

2 つ目は、検査が穴を拾えることを確かめるために、わざと壊したもの。
`terraform validate` も `fmt -check` も、`moved` の有無では落ちない。

**覆す条件**: NAT や VPC エンドポイントを足すことになったら、選択肢 C を検討し直す。
2 つ目の環境 (stg など) を作ることになったら、SG も含めた「環境の module」を考える。

## moved の確かめ方 (次に組み替えるとき用)

1. 組み替える**前**の `infra/terraform/*.tf` と `.terraform.lock.hcl` を `before/` に写す
2. 組み替えた**後**の構成 (`modules/` 含む) を `after/` に写し、`after/tests/moved.tftest.hcl` を置く。
   中身は `tests/network.tftest.hcl` の `mock_provider` (罠 2 の `mock_resource` を足したもの) と、次の 2 つの run:

   ```hcl
   run "before" {
     command   = apply
     state_key = "main"
     module { source = "../before" }
   }

   run "after" {
     command   = plan
     state_key = "main"
   }
   ```

3. `cd after && terraform init -backend=false && terraform test -verbose -no-color`
4. run "after" の `Plan:` 行が `0 to add, 0 to change, 0 to destroy` で、`has moved to` が移した件数だけ並べば OK。
   **test は差分があっても pass する** (罠 1)。必ず `Plan:` の行を読む

旧構成の写しが要るので、これはリポジトリにも CI にも入れていない (「引き受けるコスト」)。

## 罠

### 1. `terraform test` は「変更が無いこと」を assert できない

`run` の `assert` が見られるのは値で、plan の差分の有無ではない。
`moved` を外した版も **テストとしては pass** した (exit 0)。判定は `-verbose` の plan の
`Plan:` 行を読んでいる。CI に入れるなら、出力を grep するか `terraform show -json` の
`resource_changes` を数える必要がある。

### 2. mock の ARN はプロバイダの検査で落ちる

`mock_provider` の既定値はランダムな文字列で、`aws_lb_listener` の `load_balancer_arn` などは
プロバイダが ARN の形を検査するので、apply の段階でエラーが並ぶ (listener・CloudFront・ECS のタスク定義・アラーム・予算など)。
ARN を返す資源 (`aws_lb`・`aws_iam_role`・`aws_sns_topic` など) に `mock_resource` で
固定の ARN を持たせると通る。`aws_iam_policy_document` の `json` も `"{}"` に固定した
(実行のたびに値が変わると、IAM のポリシーに差分が出る)。

### 3. `moved` を消す時期

`moved` は state の中の住所を書き換えるだけで、AWS には何も送らない。
消してよいのは、**この構成で apply された state がすべて移り終わってから**。
早く消すと、古い state を持つ環境で上の「12 件作り直し」が起きる。
このリポジトリの state は手元の 1 つだけで、しかも空なので、**実は今すぐ消しても何も起きない。**
それでも残しているのは、組み替えの記録として読めるようにするため。次に network まわりを触るときに消す。

### 4. CIDR の切り方には上限と下限がある

VPC の CIDR を 8 ビット細かく切り、パブリックは 0 番から、プライベートは 10 番から取る。
**AZ が 11 個以上だと、public[10] と private[0] が同じ CIDR になる。**
VPC が /20 より小さいと、サブネットが AWS の下限 (/28) を割る。どちらも plan では通り、apply で落ちる。
module の変数の検査で止める (AZ は 1〜10 個、VPC は /16〜/20)。`tests/network.tftest.hcl` で、検査が効いていることを見ている。

## レビューで見つかったこと (PR #76、2026-11-17)

| # | 指摘 | 対応 |
| --- | --- | --- |
| 1 | AZ を「使える AZ の先頭から」選ぶので、AZ の一覧が変わるとサブネットの AZ がずれ、作り直しになる | 直した。ルートに `availability_zones` (任意) を足し、指定すれば固定できる。空なら従来どおり。この形は module 化の前から同じ |
| 2 | CIDR の切り方の都合で、AZ が 11 個以上や小さい VPC は apply で落ちる | 直した (罠 4) |
| 3 | module の AZ の下限が 1 で、ALB と RDS が要る 2 を下回る | **直さなかった。** 2 つ以上が要るのは ALB と RDS の制約で、module の制約ではない。呼び出し側の `az_count` と `availability_zones` の検査で止めている。module に 2 を書くと、module が載せるものを知ることになる |
| 4 | module に Terraform とプロバイダの版の制約が無い | 直した。ルートと同じ `~> 1.11` / `~> 6.0` |
| 5 | module のコメントにアプリの用途 (ALB と ECS / RDS) と固定の CIDR が書いてあった | 直した。CIDR は例として書き、用途は消した |
| 6 | ADR 0024 の内訳の表に `count = var.az_count` が残っていた | 直した |
| 7 | `moved` が正しいことを確かめる仕組みがリポジトリにも CI にも無い | **一部だけ直した。** 手順を上の「moved の確かめ方」に書いた。旧構成の写しが要るので、CI には入れていない。代わりに、写しの要らない検査 (AZ の選び方・module の入力) を `tests/network.tftest.hcl` に入れ、`make tf-test` と CI の Terraform Checks で回す |

## まだ確かめていないこと

- **本物の state に対する plan は、もう確かめられない。** 手元の state は destroy 後で空なので、
  次に `make tf-apply` しても最初から module のアドレスで作られ、`moved` は何もしない。
  旧アドレスで apply された state を持つ環境はほかに無い。確かめたのは mock の state だけ
- **実際の AWS の値で、差分が出ないこと。** mock の plan は、属性が mock の値どうしで一致することまで。
  AZ の選び方 (既定は先頭 2 つ、指定すればそれ) は `tests/network.tftest.hcl` の mock で見た。
  本物の `aws_availability_zones` が返す並びとは照らしていない

## 引き受けるコスト

- **ネットワークの定義が 2 か所に分かれる。** VPC を見に行くと `modules/network`、
  SG を見に行くと `network.tf`。`network.tf` の冒頭のコメントで案内している
- **`moved` ブロックを 8 つ抱える。** 今は記録のためだけにある (罠 3)
- **`moved` を確かめる道具はリポジトリに入れていない。** 旧構成の写しが要るので、
  一度きりの確認になる。次に組み替えるときは、「moved の確かめ方」をやり直す
- **mock の値を保つ。** `tests/network.tftest.hcl` の `mock_provider` は、data source の形が変わったら直す必要がある
- **資源の数え方が変わる。** `infra/terraform/*.tf` を grep すると `resource` は 62 件しか出ない。
  8 件は `modules/network` にある ([ADR 0024](0024-aws-deployment.md)「作られるリソースは 75 件」)
