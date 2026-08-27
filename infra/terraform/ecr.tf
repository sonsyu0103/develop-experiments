# コンテナイメージの置き場。
#
# **force_delete を true にする。**
# ECR はイメージが 1 つでも残っていると destroy が失敗する。
# 「使う日だけ立てる」運用では、消せないリポジトリが 1 つあるだけで
# terraform destroy が途中で止まり、**消し忘れたリソースが課金され続ける。**
#
# 本番では false にする (誤操作でイメージを全部消せてしまうため)。

locals {
  ecr_repositories = {
    api = "${local.name}-go-api"
    web = "${local.name}-next-app"

    # **マイグレーションも 1 つのイメージにする。**
    # 手元では migrate/migrate に db/migrations をボリュームで
    # マウントしているが、ECS にホストのディレクトリは無い。
    # マイグレーションファイルを焼き込んだイメージを作る
    # (apps/go-api/db/Dockerfile)。
    migrate = "${local.name}-migrate"
  }
}

resource "aws_ecr_repository" "this" {
  for_each = local.ecr_repositories

  name         = each.value
  force_delete = true

  # **push のたびに脆弱性スキャンをかける。**
  # go.mod は govulncheck が CI で見ているが、
  # ベースイメージ (alpine / distroless) 側は CI では見ていない。
  image_scanning_configuration {
    scan_on_push = true
  }

  # イメージタグは上書き可能にする。CD が同じタグ (latest) を
  # 押し直す運用のため。**本番ではダイジェスト固定のほうが安全**だが、
  # その場合はタスク定義もダイジェストで書くことになる。
  image_tag_mutability = "MUTABLE"
}

# **古いイメージを溜めない。**
# ECR は 1 GB あたり月 $0.10。放っておくと積み上がる唯一のリソースになる
# (他は destroy で消えるが、ECR は立て直しのたびに増える)。
resource "aws_ecr_lifecycle_policy" "this" {
  for_each = aws_ecr_repository.this

  repository = each.value.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "直近 10 個だけ残す"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 10
      }
      action = { type = "expire" }
    }]
  })
}
