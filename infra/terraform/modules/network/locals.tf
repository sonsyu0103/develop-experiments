locals {
  # サブネットは VPC CIDR から切り出す。**手で CIDR を並べない** ——
  # AZ の数を変えたときに、書き換え漏れで重複するのを避ける。
  #
  # VPC の CIDR を 8 ビット細かく切り、パブリックは 0 番から、プライベートは 10 番から取る。
  # 例: vpc_cidr = 10.0.0.0/16、AZ 2 つなら
  #
  #   public  : 10.0.0.0/24, 10.0.1.0/24
  #   private : 10.0.10.0/24, 10.0.11.0/24
  #
  # 10 番ずらしているので、AZ が 11 個以上だと public と private が重なる
  # (variables.tf の検査で止める)。
  public_subnet_cidrs = [
    for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnet_cidrs = [
    for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 8, i + 10)
  ]
}
