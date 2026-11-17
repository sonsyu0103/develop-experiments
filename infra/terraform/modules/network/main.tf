# VPC・サブネット・ルーティング。
#
# **この module はアプリを知らない。** 何が載るか (ALB・ECS・RDS) と、
# 誰が誰に話せるか (Security Group) は呼び出し側に残す。
# ここにあるのは「CIDR と AZ を渡すと、パブリックとプライベートのサブネットが
# AZ の数だけできる」ことだけ。
#
# NAT Gateway を置かない理由は、呼び出し側の network.tf の冒頭と ADR 0024。

resource "aws_vpc" "main" {
  cidr_block = var.vpc_cidr

  # RDS のエンドポイントを名前で引くために要る。
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = { Name = var.name }
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = var.name }
}

# ---------------------------------------------------------------------------
# サブネット
# ---------------------------------------------------------------------------

resource "aws_subnet" "public" {
  count = length(var.availability_zones)

  vpc_id            = aws_vpc.main.id
  cidr_block        = local.public_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  # **タスクにパブリック IP を配る。** これが NAT の代わりになる。
  map_public_ip_on_launch = true

  tags = { Name = "${var.name}-public-${count.index}" }
}

resource "aws_subnet" "private" {
  count = length(var.availability_zones)

  vpc_id            = aws_vpc.main.id
  cidr_block        = local.private_subnet_cidrs[count.index]
  availability_zone = var.availability_zones[count.index]

  tags = { Name = "${var.name}-private-${count.index}" }
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

  tags = { Name = "${var.name}-public" }
}

resource "aws_route_table_association" "public" {
  count = length(var.availability_zones)

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# **プライベート用のルートテーブルを明示的に作る。**
# 作らないと VPC のデフォルトルートテーブルが使われる。
# 動作は同じ (VPC 内のみ到達) だが、デフォルトに何かが足された日に
# 意図せずプライベートサブネットまで巻き込まれる。
resource "aws_route_table" "private" {
  vpc_id = aws_vpc.main.id
  tags   = { Name = "${var.name}-private" }
}

resource "aws_route_table_association" "private" {
  count = length(var.availability_zones)

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}
