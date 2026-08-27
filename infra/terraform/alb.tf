# Application Load Balancer。
#
# ── **リスナーをポートで分ける** ───────────────────────────
#
# 手元の Caddy は `handle_path /api/*` で**プレフィックスを剥がして**
# go-api へ渡している。**ALB は同じことができない** ——
# リスナールールにパスの書き換えが無い (リダイレクトはできるが、
# それでは API のリクエストにならない)。
#
# 取れる形は 3 つあった:
#
#   1. go-api 側で /api プレフィックスを受ける
#      → OpenAPI の servers とローカル構成まで変わる。影響が広い
#   2. CloudFront にオリジンを 2 つ置き、カスタムヘッダで ALB 側を振り分ける
#      → 動くが、経路がヘッダ 1 本に依存して読み解きにくい
#   3. **ALB のリスナーをポートで分ける** ← これを採る
#      → 80 は next-app、8080 は go-api。ルールが 1 つも要らない
#
# CloudFront 側で `/api/*` を 8080 のオリジンへ向け、
# **CloudFront Function でプレフィックスを剥がす** (cloudfront.tf)。
# 剥がす場所が Caddy から CloudFront に移るだけで、go-api から見た形は同じになる。
#
# リスナーの追加に料金はかからない。

resource "aws_lb" "main" {
  name               = local.name
  load_balancer_type = "application"
  subnets            = aws_subnet.public[*].id
  # **SG を 2 つ付ける。** 評価はすべての SG の和集合になる。
  # 分けている理由は network.tf (プレフィックスリストがルール枠を
  # 46 個ぶん消費するため)。
  security_groups = [aws_security_group.alb_web.id, aws_security_group.alb_api.id]

  # **インターネット向けにする。** CloudFront は VPC の外から来るので
  # internal では届かない。代わりに SG で CloudFront のエッジだけに絞る
  # (network.tf)。
  internal = false

  # destroy を止めないための設定。本番では true にする。
  enable_deletion_protection = false

  tags = { Name = local.name }
}

# ---------------------------------------------------------------------------
# ターゲットグループ
# ---------------------------------------------------------------------------
#
# **target_type = "ip"** にする。Fargate は awsvpc モードで
# タスクごとに ENI を持つため、instance では登録できない。

resource "aws_lb_target_group" "api" {
  name        = "${local.name}-api"
  port        = 8080
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip"

  health_check {
    path = "/healthz"
    # **/readyz ではなく /healthz を見る。**
    # readyz は DB への到達性まで見るので、DB が一時的に詰まると
    # タスクが入れ替わり続ける (再起動しても DB は直らない)。
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # **登録解除の待ち時間を縮める。** 既定 300 秒は
  # 「立てて壊す」運用だと destroy がそのぶん待たされる。
  deregistration_delay = 30
}

resource "aws_lb_target_group" "web" {
  name        = "${local.name}-web"
  port        = 3000
  protocol    = "HTTP"
  vpc_id      = aws_vpc.main.id
  target_type = "ip"

  health_check {
    path                = "/"
    matcher             = "200"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  deregistration_delay = 30
}

# ---------------------------------------------------------------------------
# リスナー
# ---------------------------------------------------------------------------
#
# **HTTPS にしない。**
# ALB に証明書を付けるには独自ドメインが要る (ACM は ALB の DNS 名に
# 証明書を出さない)。ここは CloudFront の既定ドメインで HTTPS を張るので、
# CloudFront ←→ ALB 間だけが HTTP になる。
#
# **その代償は X-Forwarded-Proto に出る** ——
# ALB は受信スキームで XFP を上書きするので、go-api には `http` が届く。
# selfOrigin() は http:// を組み立て、csrfGuard は Origin (https://...) と
# 一致せず **全書き込みを 403 にする**。
#
# 手元で実測したとおり (ADR 0023 の 3)、
# **CORS_ALLOWED_ORIGINS に CloudFront のドメインを明示すれば通る。**
# isAllowedOrigin は許可リストを先に見るので、selfOrigin まで到達しない。
# ecs.tf でその値を渡している。
#
# 独自ドメインを取る日に、ここを 443 + ACM に変えれば XFP も https になる。

resource "aws_lb_listener" "web" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }
}

resource "aws_lb_listener" "api" {
  load_balancer_arn = aws_lb.main.arn
  port              = 8080
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}
