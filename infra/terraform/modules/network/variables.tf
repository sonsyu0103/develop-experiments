variable "name" {
  description = "Name タグの接頭辞"
  type        = string
}

variable "vpc_cidr" {
  description = "VPC の CIDR"
  type        = string
}

# **AZ の名前を受け取る。数ではなく。**
# data source をこの中で引くと、どの AZ になるかが module の外から見えず、
# テストで差し替えることもできない。選ぶのは呼び出し側。
variable "availability_zones" {
  description = "サブネットを置く AZ の名前。この数だけパブリックとプライベートを 1 つずつ作る"
  type        = list(string)

  validation {
    condition     = length(var.availability_zones) > 0
    error_message = "availability_zones は 1 つ以上要る。"
  }
}
