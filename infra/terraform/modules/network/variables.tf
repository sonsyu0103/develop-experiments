variable "name" {
  description = "Name タグの接頭辞"
  type        = string
}

variable "vpc_cidr" {
  description = "VPC の CIDR (/16〜/20)"
  type        = string

  # 8 ビット細かく切るので、/20 より小さい VPC だとサブネットが /28 (AWS の下限) を割る。
  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && tonumber(split("/", var.vpc_cidr)[1]) >= 16 && tonumber(split("/", var.vpc_cidr)[1]) <= 20
    error_message = "vpc_cidr は /16〜/20 の CIDR にする (サブネットは 8 ビット細かい /24〜/28 になる)。"
  }
}

# **AZ の名前を受け取る。数ではなく。**
# data source をこの中で引くと、どの AZ になるかが module の外から見えず、
# テストで差し替えることもできない。選ぶのは呼び出し側。
#
# **下限は 1 にしてある (2 ではなく)。** 2 AZ 以上が要るのは ALB と RDS の制約で、
# この module ではなく、それを載せる側の都合。呼び出し側 (az_count) で検査している。
variable "availability_zones" {
  description = "サブネットを置く AZ の名前。この数だけパブリックとプライベートを 1 つずつ作る"
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) >= 1 && length(var.availability_zones) <= 10
    error_message = "availability_zones は 1〜10 個 (11 個以上だと public と private の CIDR が重なる)。"
  }
}
