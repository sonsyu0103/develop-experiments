locals {
  name = var.project

  # サブネットは VPC CIDR から切り出す。**手で CIDR を並べない** ——
  # az_count を変えたときに、書き換え漏れで重複するのを避ける。
  #
  #   public  : 10.0.0.0/24, 10.0.1.0/24    (ALB と ECS タスク)
  #   private : 10.0.10.0/24, 10.0.11.0/24  (RDS だけ)
  public_subnet_cidrs = [
    for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnet_cidrs = [
    for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 8, i + 10)
  ]
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
