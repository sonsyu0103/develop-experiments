# AWS の構成 (Terraform)

判断の背景は [ADR 0024](../../docs/adr/0024-aws-deployment.md)。
ここには**手順**だけ書く。

## 前提

```bash
brew install hashicorp/tap/terraform awscli
aws configure     # または SSO / 環境変数
aws sts get-caller-identity   # 通ることを確認する
```

**このコードは課金の発生するリソースを作る。** 常時起動で月 $39 ほど
(内訳は ADR 0024)。使う日だけ立てて、終わったら消す運用にしてある。

## 立てる

```bash
make tf-init      # 最初の 1 回
make tf-plan      # 何が作られるかを見る (課金なし)
make tf-apply     # 作る。RDS があるので 15 分ほどかかる
make tf-push      # イメージを ECR へ (ARM64)
make tf-migrate   # マイグレーションを流す
make tf-url       # 公開 URL
```

**順番に意味がある。** `tf-apply` の時点では ECR が空なので、
ECS のタスクは起動できずに再試行を続ける。`tf-push` で
イメージが入って初めて健全になる。

CloudFront の配信が行き渡るまで数分かかるので、
apply 直後に 403 や 404 が返っても、少し待ってから見直す。

## 消す

```bash
make tf-destroy
```

**消し忘れがいちばん怖い。** AWS Budgets を仕掛けてあるが
(`monitoring.tf`)、通知が届く頃には使ってしまっている。
`terraform destroy` が完走したことを確認する。

消えたかどうかは、タグで確認できる:

```bash
aws resourcegroupstaggingapi get-resources \
  --tag-filters Key=Project,Values=bbs --query 'ResourceTagMappingList[].ResourceARN'
```

## 更新する

```bash
make tf-deploy    # push してサービスを入れ替え、安定するまで待つ
```

GitHub Actions からも流せる (`.github/workflows/deploy.yml`)。
`make tf-apply` の出力にあるロール ARN などを、リポジトリの
Variables に設定する:

| Variable | 値の取り方 |
| --- | --- |
| `AWS_DEPLOY_ROLE_ARN` | `terraform output -raw github_actions_role_arn` |
| `ECS_CLUSTER` | `terraform output -raw ecs_cluster_name` |
| `ECS_SERVICE_API` / `ECS_SERVICE_WEB` | `terraform output -json ecs_service_names` |
| `ECS_TASK_MIGRATE` | `terraform output -raw migrate_task_definition` |
| `ECR_REPO_API` / `ECR_REPO_WEB` / `ECR_REPO_MIGRATE` | `terraform output -json ecr_repository_urls` の末尾 |
| `CLOUDFRONT_DOMAIN` | `terraform output -raw public_url` からホスト部分 |

## 設定

`terraform.tfvars.example` をコピーして使う。**すべて任意**で、
何も設定しなくても apply は通る (ログインだけが 503 になる)。

Google OIDC を使うなら、**Google 側にリダイレクト URI を登録する**。
`make tf-url` で出た URL の `/api/auth/google/callback` を登録し、
`google_client_id` / `google_client_secret` を tfvars に入れて apply し直す。

## つまずきやすいところ

| 症状 | 原因 |
| --- | --- |
| タスクが起動しては落ちる | イメージが x86。ARM64 で焼き直す (`make tf-push`) |
| 画面は出るが真っ白 | `API_URL` が CloudFront 経由になっているか確認 (`ecs.tf`)。**SG は疑わない** —— internet-facing ALB へは VPC の外を通るので、SG 参照は効かない (ADR 0024 の 12) |
| 書き込みだけ 403 | `CORS_ALLOWED_ORIGINS` が CloudFront のドメインと違う (ADR 0024 の 2) |
| apply が `already scheduled for deletion` | Secrets Manager の復旧猶予。`recovery_window_in_days = 0` になっているか確認 |
| destroy が途中で止まる | S3 に中身、ECR にイメージが残っている。`force_destroy` / `force_delete` を確認 |

## state について

**backend は local。** DB のパスワードを Terraform に生成させているので、
`terraform.tfstate` には平文で入る。`.gitignore` 済みだが、
**state 自体を秘密として扱う。**

チームで共有するなら `versions.tf` の backend block を
S3 に差し替える (Terraform 1.10 以降は `use_lockfile` で
DynamoDB のロックテーブルが要らない)。
