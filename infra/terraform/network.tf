# ネットワーク。
#
# ── **NAT Gateway を置かない** ─────────────────────────────
#
# 教科書どおりの構成では ECS タスクをプライベートサブネットに置き、
# 外向き通信 (ECR からのイメージ取得、Google OIDC のトークン交換、SES)
# を NAT Gateway に通す。
#
# だが NAT は**起動しているだけで月 $33 前後**かかり、
# この構成でいちばん高い部品になる —— ALB ($18) より高い。
# 「必要な日だけ立てる」運用でも、立てている間ずっと課金される。
#
# ここでは ECS タスクを**パブリックサブネットに置き、
# パブリック IP を付けて直接インターネットへ出す。**
# 受信は Security Group で ALB からのみに絞るので、
# **パブリック IP が付いていることと、外から入れることは別**になる。
#
# 本番でこうしない理由と代替案 (VPC エンドポイント / NAT) は
# docs/adr/0024-aws-deployment.md に書く。
#
# RDS だけはプライベートサブネットに置く。
# RDS は外向き通信が要らないので、NAT が無くても困らない。
#
# ── **description だけ英語なのは AWS の制約** ──────────────
#
# EC2 API は Security Group とそのルールの description に
# **ASCII しか受け付けない** (日本語を入れると
# InvalidParameterValue: Character sets beyond ASCII are not supported)。
#
# **terraform validate では落ちない。** API に投げて初めて分かるので、
# apply の途中で 3 つ同時に失敗した (ADR 0024 の 8)。
# 説明はコメント側に日本語で残し、API に送る値だけ英語にしている。
#
# CloudFront の comment や CloudWatch の alarm_description は
# UTF-8 を受け付けるので、そちらは日本語のままでよい。

resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr

  # RDS のエンドポイントを名前で引くために要る。
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = local.name }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = local.name }
}

# ---------------------------------------------------------------------------
# サブネット
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count = var.az_count

  vpc_id            = aws_vpc.main.id
  cidr_block        = local.public_subnet_cidrs[count.index]
  availability_zone = data.aws_availability_zones.available.names[count.index]

  # **タスクにパブリック IP を配る。** これが NAT の代わりになる。
  map_public_ip_on_launch = true

  tags = { Name = "${local.name}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count = var.az_count

  vpc_id            = aws_vpc.main.id
  cidr_block        = local.private_subnet_cidrs[count.index]
  availability_zone = data.aws_availability_zones.available.names[count.index]

  tags = { Name = "${local.name}-private-${count.index}" }
}

# ---------------------------------------------------------------------------
# ルーティング
# ---------------------------------------------------------------------------

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }

  tags = { Name = "${local.name}-public" }
}

resource "aws_route_table_association" "public" {
  count = var.az_count

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# **プライベート用のルートテーブルを明示的に作る。**
# 作らないと VPC のデフォルトルートテーブルが使われる。
# 動作は同じ (VPC 内のみ到達) だが、デフォルトに何かが足された日に
# 意図せずプライベートサブネットまで巻き込まれる。
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${local.name}-private" }
}

resource "aws_route_table_association" "private" {
  count = var.az_count

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ---------------------------------------------------------------------------
# Security Group
# ---------------------------------------------------------------------------

# **CloudFront からの受信だけを許す。**
#
# ALB をインターネットに向けて置くが、0.0.0.0/0 は開けない。
# AWS が管理するプレフィックスリストを使うと、CloudFront のエッジ IP
# レンジだけを許可できる —— **IP レンジが変わっても追随される**ので、
# 自分で表を保守しなくてよい。
#
# これで「ALB の DNS 名を直接叩いて CloudFront を迂回する」経路が塞がる。
# 迂回されると、CloudFront で付けているセキュリティヘッダも
# WAF (将来足すなら) も素通しになる。
data "aws_ec2_managed_prefix_list" "cloudfront_origin_facing" {
  name = "com.amazonaws.global.cloudfront.origin-facing"
}

# ── **ALB の SG を 2 つに分ける理由** ──────────────────────
#
# 見た目は 1 行でも、**プレフィックスリストは含まれる CIDR の数だけ
# ルール枠を消費する。** CloudFront のリストは 46 個あり、
# Security Group あたりのルール上限は既定で 60。
#
# 80 番と 8080 番の 2 箇所で使うと 46 x 2 + 2 = 94 になり、
# **apply の途中で RulesPerSecurityGroupLimitExceeded になる**
# (ADR 0024 の 9)。
#
# ALB には SG を複数アタッチでき、評価は**すべての SG の和集合**になる。
# 用途ごとに分ければ、それぞれが 60 以内に収まる:
#
#   alb_web : 80 番 (46 + egress 1)      = 47
#   alb_api : 8080 番 (46 + ECS 1 + egress 1) = 48
#
# クォータ引き上げ (60 -> 120) を申請する手もあるが、
# **申請の承認を待たずに済むほうを採る。**
# 用途で分かれているぶん、読みやすさも上がる。

resource "aws_security_group" "alb_web" {
  name        = "${local.name}-alb-web"
  description = "ALB web listener. HTTP from CloudFront edge only"
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-alb-web" }
}

resource "aws_vpc_security_group_ingress_rule" "alb_from_cloudfront" {
  security_group_id = aws_security_group.alb_web.id
  description       = "From CloudFront edge locations"

  ip_protocol    = "tcp"
  from_port      = 80
  to_port        = 80
  prefix_list_id = data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.id
}

resource "aws_vpc_security_group_egress_rule" "alb_web_all" {
  security_group_id = aws_security_group.alb_web.id
  description       = "Forward to targets"

  ip_protocol = "-1"
  cidr_ipv4   = "0.0.0.0/0"
}

resource "aws_security_group" "alb_api" {
  name        = "${local.name}-alb-api"
  description = "ALB api listener. From CloudFront edge and ECS tasks"
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-alb-api" }
}

resource "aws_vpc_security_group_ingress_rule" "alb_api_from_cloudfront" {
  security_group_id = aws_security_group.alb_api.id
  description       = "From CloudFront edge to the API listener"

  ip_protocol    = "tcp"
  from_port      = 8080
  to_port        = 8080
  prefix_list_id = data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.id
}

# **ECS から ALB への ingress は置かない。**
#
# 当初は「next-app の Server Components が go-api を叩く経路」として
# 開けていたが、**この許可は成立しない** (ADR 0024 の 12):
#
#   internet-facing な ALB の DNS 名は VPC の中から引いてもパブリック IP を
#   返し、パブリック IP を持つ ECS タスクからの通信は
#   **いったんインターネットに出て戻る**。ALB から見た送信元は
#   「ECS のパブリック IP」であって Security Group ではないので、
#   referenced_security_group_id による許可は効かない。
#
# いまは API_URL が CloudFront を経由する (ecs.tf)。
# **効かない規則を残さない** —— プレフィックスリストで
# 46/60 まで埋まっている SG では、1 枠も無駄にできない。

resource "aws_vpc_security_group_egress_rule" "alb_api_all" {
  security_group_id = aws_security_group.alb_api.id
  description       = "Forward to targets"

  ip_protocol = "-1"
  cidr_ipv4   = "0.0.0.0/0"
}

# ECS タスク。**受信は ALB からのみ。**
# パブリックサブネットに居てパブリック IP も付いているが、
# ここで塞いでいるので外からは入れない。
resource "aws_security_group" "ecs" {
  name        = "${local.name}-ecs"
  description = "ECS tasks. Ingress from ALB only, egress allowed"
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-ecs" }
}

resource "aws_vpc_security_group_ingress_rule" "ecs_api_from_alb" {
  security_group_id = aws_security_group.ecs.id
  description       = "Forward to go-api"

  ip_protocol                  = "tcp"
  from_port                    = 8080
  to_port                      = 8080
  referenced_security_group_id = aws_security_group.alb_api.id
}

resource "aws_vpc_security_group_ingress_rule" "ecs_web_from_alb" {
  security_group_id = aws_security_group.ecs.id
  description       = "Forward to next-app"

  ip_protocol                  = "tcp"
  from_port                    = 3000
  to_port                      = 3000
  referenced_security_group_id = aws_security_group.alb_web.id
}

# **外向きは開ける。** NAT を置かない代わりに、
# ここから ECR / Google OIDC / SES へ直接出る。
resource "aws_vpc_security_group_egress_rule" "ecs_all" {
  security_group_id = aws_security_group.ecs.id
  description       = "Outbound to ECR / Google OIDC / SES"

  ip_protocol = "-1"
  cidr_ipv4   = "0.0.0.0/0"
}

# RDS。**ECS からのみ。外向きは開けない。**
resource "aws_security_group" "rds" {
  name        = "${local.name}-rds"
  description = "RDS. Ingress from ECS tasks only"
  vpc_id      = aws_vpc.main.id

  tags = { Name = "${local.name}-rds" }
}

resource "aws_vpc_security_group_ingress_rule" "rds_from_ecs" {
  security_group_id = aws_security_group.rds.id
  description       = "PostgreSQL"

  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.ecs.id
}
