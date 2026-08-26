resource "aws_ecs_cluster" "main" {
  name = local.name

  # Container Insights は**有効にしない。**
  # メトリクスは CloudWatch の従量課金で、立てているだけで積み上がる。
  # 見たいのは「動くこと」であって、詳細メトリクスではない。
  setting {
    name  = "containerInsights"
    value = "disabled"
  }
}

# **Fargate Spot を既定にする。** 通常の Fargate との差は約 70%。
# 中断されうるが、この用途では落ちても取り返しがつく。
resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = var.use_fargate_spot ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
  }
}

# ---------------------------------------------------------------------------
# ログ
# ---------------------------------------------------------------------------
#
# **awslogs で CloudWatch に出す。**
#
# ADR 0010 は「Fluent Bit (FireLens) → S3 → Athena」を決めているが、
# ここでは採らない。理由は 2 つ:
#
#   1. FireLens はタスクにサイドカーを 1 つ足す。**タスクあたりの
#      メモリが増え、Spot の割り当ても取りにくくなる**
#   2. S3 に貯めたログを Athena で読むのは、**貯まってから**意味を持つ。
#      立てて壊してを繰り返す環境では、貯まる前に消える
#
# まず動かすことを優先し、ログ基盤は段階を分ける
# (docs/adr/0024-aws-deployment.md「やらないこと」)。
#
# **保持期間を明示する。** 既定 (無期限) だと、消し忘れたロググループが
# 課金され続ける —— destroy で消えるので実害は小さいが、
# 「意図して選んだ」ことを構成に残す。

resource "aws_cloudwatch_log_group" "api" {
  name              = "/ecs/${local.name}/go-api"
  retention_in_days = 7
}

resource "aws_cloudwatch_log_group" "web" {
  name              = "/ecs/${local.name}/next-app"
  retention_in_days = 7
}

resource "aws_cloudwatch_log_group" "migrate" {
  name              = "/ecs/${local.name}/migrate"
  retention_in_days = 7
}

# ---------------------------------------------------------------------------
# タスク定義
# ---------------------------------------------------------------------------

locals {
  # **公開 URL は CloudFront のドメイン。**
  # apply するまで決まらないので、環境変数は参照で組み立てる。
  public_url = "https://${aws_cloudfront_distribution.main.domain_name}"

  image = {
    for k, repo in aws_ecr_repository.this : k => "${repo.repository_url}:${
      k == "api" ? var.api_image_tag : (k == "web" ? var.web_image_tag : "latest")
    }"
  }
}

resource "aws_ecs_task_definition" "api" {
  family                   = "${local.name}-go-api"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512

  execution_role_arn = aws_iam_role.ecs_execution.arn
  task_role_arn      = aws_iam_role.ecs_task.arn

  # Graviton。**x86 より 2 割ほど安い。**
  # go-api は distroless + static ビルドなので、
  # GOARCH=arm64 で焼けば動く (CD 側で指定する)。
  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "go-api"
    image     = local.image["api"]
    essential = true

    portMappings = [{
      containerPort = 8080
      protocol      = "tcp"
    }]

    environment = [
      { name = "ENV", value = "production" },
      { name = "ADDR", value = ":8080" },

      # ログ基盤に流す形 (ADR 0010)。CloudWatch に出す場合も
      # JSON のままにして、手元との差を作らない。
      { name = "LOG_FORMAT", value = "json" },

      # **CORS_ALLOWED_ORIGINS を必ず明示する。**
      #
      # 単一オリジン構成なので本来は不要に見えるが、**この構成では必須**になる。
      # CloudFront ←→ ALB が HTTP なので、ALB が付ける X-Forwarded-Proto は
      # `http` になり、selfOrigin() は http:// を組み立てる。
      # 許可リストに CloudFront のドメインが無いと、
      # **すべての書き込みが 403 になる** (ADR 0023 の 3 で実測した形)。
      { name = "CORS_ALLOWED_ORIGINS", value = local.public_url },

      # **ALB と CloudFront のエッジを信頼するプロキシとして宣言する。**
      #
      # 宣言しないと ClientIP がプロキシの IP になり、問い合わせの
      # レート制限 (5 件/時) が全利用者で共有される —— 誰か 5 人で
      # 全員が 429 (ADR 0023、手元で実測)。
      #
      # **VPC CIDR だけでは足りない。** XFF は
      # `viewer, cloudfront-edge` の形で届き、gin は右から辿るので、
      # エッジを信頼しないとエッジ IP で止まる (locals.tf の注記)。
      { name = "TRUSTED_PROXIES", value = join(",", local.trusted_proxies) },

      # ENV=production でも既定で true になるが、**明示する。**
      # この値が外れると Cookie が平文で飛ぶ。既定に頼らない。
      { name = "SECURE_COOKIE", value = "true" },

      { name = "AUTH_REDIRECT_URL", value = "${local.public_url}/api/auth/google/callback" },
      { name = "AUTH_FRONTEND_URL", value = local.public_url },

      { name = "S3_BUCKET", value = aws_s3_bucket.images.bucket },
      { name = "S3_REGION", value = var.region },
      # **末尾に /images を付けない。**
      # オブジェクトキー自体が "images/<uuid>.webp" で始まる
      # (image/domain/model/image.go)。付けると
      # https://<cf>/images/images/<uuid>.webp になり、
      # CloudFront はプレフィックスを剥がさないので S3 が 403 を返す。
      #
      # 手元の Caddy は handle_path が /images を剥がしてから
      # /bbs-images を足すので、同じ値でも成立していた ——
      # **その値をそのまま持ってきたのが原因。**
      { name = "S3_PUBLIC_BASE_URL", value = local.public_url },

      # 認証は任意 (未設定ならログインの 2 経路だけが 503。ADR 0005)。
      { name = "GOOGLE_CLIENT_ID", value = var.google_client_id },
      { name = "GOOGLE_CLIENT_SECRET", value = var.google_client_secret },
    ]

    # **DATABASE_URL は Secrets Manager から注入する。**
    # environment に書くと、タスク定義を読める人全員に見える。
    secrets = [{
      name      = "DATABASE_URL"
      valueFrom = "${aws_secretsmanager_secret.db.arn}:url::"
    }]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.api.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "ecs"
      }
    }
  }])
}

resource "aws_ecs_task_definition" "web" {
  family                   = "${local.name}-next-app"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512

  execution_role_arn = aws_iam_role.ecs_execution.arn
  task_role_arn      = aws_iam_role.ecs_task.arn

  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "next-app"
    image     = local.image["web"]
    essential = true

    portMappings = [{
      containerPort = 3000
      protocol      = "tcp"
    }]

    environment = [
      { name = "NODE_ENV", value = "production" },

      # **Server Components からの呼び出しも CloudFront を回す。**
      #
      # 手元では `http://go-api:8080` とサービス名で解決していた。
      # 当初はその置き換えとして ALB を直に叩かせたが、**繋がらなかった**
      # (ADR 0024 の 12):
      #
      #   1. internet-facing な ALB の DNS 名は、**VPC の中から引いても
      #      パブリック IP を返す**
      #   2. ECS タスクはパブリック IP を持つので、そこへの通信は
      #      **いったんインターネットに出て戻る**
      #   3. ALB から見た送信元は「ECS のパブリック IP」であって
      #      **ECS の Security Group ではない**
      #   4. referenced_security_group_id による許可は VPC 内の通信にしか
      #      効かないので、拒否されてタイムアウトする
      #
      # SG の書き方ではなく、**経路そのものが VPC の外を通っていた。**
      #
      # CloudFront は誰からでも受けるので、こちらなら通る。
      # 往復は増えるが、**Server Components からの取得は GET だけ**なので
      # csrfGuard にも当たらない (ADR 0013 の 7)。
      #
      # **本来は ECS Service Connect を使うべき場面になる。**
      # go-api を `http://go-api:8080` の名前で引けるようになり、
      # 手元の compose と同じ形に揃う。追加料金もかからない。
      # いまは動かすことを優先し、ADR 0024「やり残し」に回す。
      { name = "API_URL", value = "${local.public_url}/api" },

      # **NEXT_PUBLIC_API_URL はここに置かない。**
      # ビルド時に定数へ置換される値なので、実行時の環境変数では
      # 上書きできない (apps/next-app/Dockerfile の ARG で渡している)。
      # ここに書くと「設定したのに効かない」変数が残り、
      # 次に読む人を確実に誤解させる。
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.web.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "ecs"
      }
    }
  }])
}

# **マイグレーションは常駐させない。**
# サービスではなくタスク定義だけ作り、`aws ecs run-task` で 1 回流す
# (make tf-migrate)。サービスにすると、失敗のたびに再起動を繰り返す。
resource "aws_ecs_task_definition" "migrate" {
  family                   = "${local.name}-migrate"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512

  execution_role_arn = aws_iam_role.ecs_execution.arn
  task_role_arn      = aws_iam_role.ecs_task.arn

  runtime_platform {
    cpu_architecture        = "ARM64"
    operating_system_family = "LINUX"
  }

  container_definitions = jsonencode([{
    name      = "migrate"
    image     = local.image["migrate"]
    essential = true

    secrets = [{
      name      = "DATABASE_URL"
      valueFrom = "${aws_secretsmanager_secret.db.arn}:url::"
    }]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = aws_cloudwatch_log_group.migrate.name
        "awslogs-region"        = var.region
        "awslogs-stream-prefix" = "ecs"
      }
    }
  }])
}

# ---------------------------------------------------------------------------
# サービス
# ---------------------------------------------------------------------------

resource "aws_ecs_service" "api" {
  name            = "${local.name}-go-api"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.api.arn
  desired_count   = 1

  capacity_provider_strategy {
    capacity_provider = var.use_fargate_spot ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
  }

  network_configuration {
    subnets         = aws_subnet.public[*].id
    security_groups = [aws_security_group.ecs.id]

    # **パブリック IP を付ける。** NAT を置かない代わりに、
    # ここから ECR / Google / SES へ出る (network.tf)。
    assign_public_ip = true
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "go-api"
    container_port   = 8080
  }

  # **ECS Exec は有効にするが、go-api には入れない。**
  # prod イメージは distroless で shell を持たないため
  # (apps/go-api/Dockerfile)。有効にしておくのは、
  # next-app 側 (alpine) には効くのと、
  # 障害時に debug イメージへ差し替えれば使えるようにするため。
  enable_execute_command = true

  # **task_definition を無視しない。**
  #
  # 当初は「CD がイメージを差し替えるから」と ignore_changes に入れていたが、
  # **環境変数を変えても届かなくなる** —— Terraform が新しいリビジョンを
  # 登録して「適用した」と報告する一方、サービスは古いリビジョンのまま走る。
  # 実際に API_URL を直したときに踏み、手で update-service する羽目になった。
  #
  # CD (deploy.yml) はイメージタグを固定 (latest) して
  # --force-new-deployment するだけで、**タスク定義は登録し直さない**。
  # だから無視する理由がそもそも無い。

  depends_on = [aws_lb_listener.api]
}

resource "aws_ecs_service" "web" {
  name            = "${local.name}-next-app"
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.web.arn
  desired_count   = 1

  capacity_provider_strategy {
    capacity_provider = var.use_fargate_spot ? "FARGATE_SPOT" : "FARGATE"
    weight            = 1
  }

  network_configuration {
    subnets          = aws_subnet.public[*].id
    security_groups  = [aws_security_group.ecs.id]
    assign_public_ip = true
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.web.arn
    container_name   = "next-app"
    container_port   = 3000
  }

  enable_execute_command = true

  # api 側と同じ理由で ignore_changes は使わない。

  depends_on = [aws_lb_listener.web]
}
