# ---------------------------------------------------------------------------
# ECS のロール
# ---------------------------------------------------------------------------
#
# **2 つに分ける。**
#   実行ロール (execution role) : ECS エージェントが使う。イメージ取得とログ出力
#   タスクロール (task role)     : アプリ自身が使う。S3 と SES
#
# 分けないと、アプリのバグで漏れた資格情報で ECR まで触れることになる。

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "ecs_execution" {
  name               = "${local.name}-ecs-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

resource "aws_iam_role_policy_attachment" "ecs_execution" {
  role       = aws_iam_role.ecs_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# **Secrets Manager の読み取りは実行ロール側に要る。**
# タスク定義の `secrets` で環境変数へ注入するのは ECS エージェントの仕事なので、
# アプリ側 (タスクロール) に権限があっても起動に失敗する。
data "aws_iam_policy_document" "ecs_execution_secrets" {
  statement {
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.db.arn]
  }
}

resource "aws_iam_role_policy" "ecs_execution_secrets" {
  name   = "read-db-secret"
  role   = aws_iam_role.ecs_execution.id
  policy = data.aws_iam_policy_document.ecs_execution_secrets.json
}

resource "aws_iam_role" "ecs_task" {
  name               = "${local.name}-ecs-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
}

# アプリが触るのは画像バケットと SES だけ。
data "aws_iam_policy_document" "ecs_task" {
  statement {
    sid    = "Images"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
    ]
    resources = ["${aws_s3_bucket.images.arn}/*"]
  }

  statement {
    sid       = "SendMail"
    effect    = "Allow"
    actions   = ["ses:SendEmail", "ses:SendRawEmail"]
    resources = ["*"]
  }

  # ECS Exec に要る。go-api (distroless) には shell が無いので入れないが、
  # next-app 側の切り分けで使う。
  statement {
    sid    = "ExecuteCommand"
    effect = "Allow"
    actions = [
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "ecs_task" {
  name   = "app"
  role   = aws_iam_role.ecs_task.id
  policy = data.aws_iam_policy_document.ecs_task.json
}

# ---------------------------------------------------------------------------
# GitHub Actions から OIDC で入る
# ---------------------------------------------------------------------------
#
# **アクセスキーを GitHub Secrets に置かない。**
# 置いた鍵は、失効させるまで有効なまま残る。OIDC なら
# ワークフローの実行ごとに短命の資格情報が発行される。

data "aws_iam_openid_connect_provider" "github" {
  count = var.create_github_oidc_provider ? 0 : 1
  url   = "https://token.actions.githubusercontent.com"
}

# **既存があれば作らない。** OIDC プロバイダはアカウントに 1 つしか作れず、
# 他のプロジェクトで既に作っていると apply が衝突する。
resource "aws_iam_openid_connect_provider" "github" {
  count = var.create_github_oidc_provider ? 1 : 0

  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]
}

locals {
  github_oidc_arn = var.create_github_oidc_provider ? aws_iam_openid_connect_provider.github[0].arn : data.aws_iam_openid_connect_provider.github[0].arn
}

data "aws_iam_policy_document" "github_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [local.github_oidc_arn]
    }

    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }

    # **リポジトリを絞る。** ここを緩めると、
    # GitHub 上の**任意のリポジトリ**からこのロールを引き受けられる。
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:*"]
    }
  }
}

resource "aws_iam_role" "github_actions" {
  name               = "${local.name}-github-actions"
  assume_role_policy = data.aws_iam_policy_document.github_assume.json
}

# CD がやるのは「イメージを push して、サービスを更新する」だけ。
data "aws_iam_policy_document" "github_actions" {
  statement {
    sid       = "EcrAuth"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  statement {
    sid    = "EcrPush"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:CompleteLayerUpload",
      "ecr:InitiateLayerUpload",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
      "ecr:BatchGetImage",
      "ecr:GetDownloadUrlForLayer",
    ]
    resources = [for r in aws_ecr_repository.this : r.arn]
  }

  # **deploy.yml が実際に呼ぶ API をすべて挙げる。**
  #
  # 手元のスクリプトは開発者の管理者権限で動くので、
  # **この不足はローカルでは一度も露見しない。**
  # DescribeClusters が欠けていると、ワークフローは最初の確認で
  # 「クラスタが見つかりません。先に make tf-apply してください」と
  # 表示して止まる —— AccessDenied が grep に吸われ、
  # **原因と違うメッセージが出る**。
  statement {
    sid    = "DeployService"
    effect = "Allow"
    actions = [
      "ecs:DescribeClusters",
      "ecs:DescribeServices",
      "ecs:UpdateService",
      "ecs:DescribeTaskDefinition",
      "ecs:RegisterTaskDefinition",
      # マイグレーションを run-task で流し、終了コードを見るために要る。
      "ecs:RunTask",
      "ecs:DescribeTasks",
    ]
    resources = ["*"]
  }

  # タスク定義を登録するには、そこに書くロールを渡す権限が要る。
  statement {
    sid       = "PassRoles"
    effect    = "Allow"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.ecs_execution.arn, aws_iam_role.ecs_task.arn]
  }
}

resource "aws_iam_role_policy" "github_actions" {
  name   = "deploy"
  role   = aws_iam_role.github_actions.id
  policy = data.aws_iam_policy_document.github_actions.json
}
