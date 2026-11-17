# ネットワーク。
#
# VPC・サブネット・ルーティングは modules/network にある (ADR 0028)。
# ここに残しているのは module の呼び出しと、アプリの経路を知っている Security Group。
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

# ---------------------------------------------------------------------------
# VPC・サブネット・ルーティング (modules/network)
# ---------------------------------------------------------------------------

module "network" {
  source = "./modules/network"

  name               = local.name
  vpc_cidr           = var.vpc_cidr
  availability_zones = slice(data.aws_availability_zones.available.names, 0, var.az_count)
}

# **module に移した資源の state を付け替える。**
#
# これが無いと Terraform は「旧アドレスの資源を消して、module の中に新しく作る」と
# 計画する —— VPC ごと作り直しになり、RDS も ALB も巻き込まれる。
# moved は state の中の住所を書き換えるだけで、AWS には何も送らない。
#
# count 付きの資源は、資源ごと書けば [0] [1] … もまとめて移る。
#
# 消してよいのは、この構成で apply された state がすべて移り終わってから。
# 早く消すと、古い state を持つ環境で上の「作り直し」が起きる。
moved {
  from = aws_vpc.main
  to   = module.network.aws_vpc.main
}

moved {
  from = aws_internet_gateway.main
  to   = module.network.aws_internet_gateway.main
}

moved {
  from = aws_subnet.public
  to   = module.network.aws_subnet.public
}

moved {
  from = aws_subnet.private
  to   = module.network.aws_subnet.private
}

moved {
  from = aws_route_table.public
  to   = module.network.aws_route_table.public
}

moved {
  from = aws_route_table_association.public
  to   = module.network.aws_route_table_association.public
}

moved {
  from = aws_route_table.private
  to   = module.network.aws_route_table.private
}

moved {
  from = aws_route_table_association.private
  to   = module.network.aws_route_table_association.private
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
  vpc_id      = module.network.vpc_id

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
  vpc_id      = module.network.vpc_id

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
  vpc_id      = module.network.vpc_id

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
  vpc_id      = module.network.vpc_id

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
