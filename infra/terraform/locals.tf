locals {
  name = var.project

  # サブネットの CIDR は modules/network/locals.tf で VPC の CIDR から切り出す。

  # 固定されていなければ、使える AZ の先頭から az_count 個 (variables.tf の availability_zones)。
  availability_zones = (
    length(var.availability_zones) > 0
    ? var.availability_zones
    : slice(data.aws_availability_zones.available.names, 0, var.az_count)
  )
}

data "aws_availability_zones" "available" {
  state = "available"
}

# **信頼するプロキシは VPC だけでは足りない。**
#
# gin の ClientIP は X-Forwarded-For を**右から辿り、最初の
# 「信頼していない IP」を返す** (validateHeader)。この構成の XFF は
#
#     viewer-ip, cloudfront-edge-ip
#                 ^ ALB が直前ピア (= CloudFront のエッジ) を追記する
#
# になる。VPC CIDR だけを信頼すると、右端のエッジ IP が
# 「信頼していない IP」として**そのまま ClientIP になる。**
#
# 何が起きるか:
#
#   - 問い合わせのレート制限 (既定 5 件/時) が、**同じ PoP を通る
#     利用者どうしで共有される** —— ALB の IP に潰れる問題を直したつもりが、
#     エッジの IP に潰れる形で残る
#   - 閲覧数の重複抑制 (ADR 0006 の visitorKey) も同じ値を使うので、
#     **同じ PoP からの閲覧が 1 人分に丸められる**
#
# エッジのレンジも信頼に含めれば、辿りが 1 つ左へ進んで viewer に届く。
# レンジは AWS が管理するプレフィックスリストから取るので、
# **自分で表を保守しなくてよい** (SG で使っているのと同じ出どころ)。
locals {
  trusted_proxies = concat(
    [var.vpc_cidr],
    [for e in data.aws_ec2_managed_prefix_list.cloudfront_origin_facing.entries : e.cidr],
  )
}
