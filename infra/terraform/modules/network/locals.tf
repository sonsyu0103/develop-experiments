locals {
  # サブネットは VPC CIDR から切り出す。**手で CIDR を並べない** ——
  # AZ の数を変えたときに、書き換え漏れで重複するのを避ける。
  #
  #   public  : 10.0.0.0/24, 10.0.1.0/24    (ALB と ECS タスク)
  #   private : 10.0.10.0/24, 10.0.11.0/24  (RDS だけ)
  public_subnet_cidrs = [
    for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 8, i)
  ]
  private_subnet_cidrs = [
    for i in range(length(var.availability_zones)) : cidrsubnet(var.vpc_cidr, 8, i + 10)
  ]
}
