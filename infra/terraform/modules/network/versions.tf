# 版の決め方はルートの versions.tf と同じ (**`~>` の上限を開けない**)。
# ルート以外から呼んだときにも、同じ範囲でしか init が通らないようにする。
terraform {
  required_version = "~> 1.11"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }
}
