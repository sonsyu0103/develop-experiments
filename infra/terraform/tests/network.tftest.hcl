# AZ の選び方と modules/network の入力の検査が効いているかを、AWS に触らずに確かめる。
#
#   make tf-test   (CI の Terraform Checks でも回す)
#
# **mock_provider は認証も課金も要らない。** data source も resource も、
# 下に書いた値かランダムな値を返す。plan だけなので、AWS の API は一度も呼ばない。
#
# **ここでは moved は確かめていない。** moved を確かめるには、組み替える前の構成で作った
# state が要る (ADR 0028「確かめ方」)。

mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = { names = ["ap-northeast-1a", "ap-northeast-1c", "ap-northeast-1d"] }
  }
  mock_data "aws_ec2_managed_prefix_list" {
    defaults = {
      id      = "pl-58a04531"
      entries = [{ cidr = "130.176.0.0/18", description = "" }]
    }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{}" }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
}

mock_provider "aws" {
  alias = "us_east_1"
}

# 既定では、使える AZ の先頭から az_count (2) 個。
run "default_azs_are_first_two" {
  command = plan

  assert {
    condition     = join(",", local.availability_zones) == "ap-northeast-1a,ap-northeast-1c"
    error_message = "既定で使える AZ の先頭 2 つになっていない: ${join(",", local.availability_zones)}"
  }
}

# 固定したら、一覧の順番に関係なくその AZ を使う。
run "pinned_azs_are_used" {
  command = plan

  variables {
    availability_zones = ["ap-northeast-1c", "ap-northeast-1d"]
  }

  assert {
    condition     = join(",", local.availability_zones) == "ap-northeast-1c,ap-northeast-1d"
    error_message = "固定した AZ が使われていない: ${join(",", local.availability_zones)}"
  }
}

# ALB は 2 AZ 以上を要る。固定するときも 1 つは通さない。
run "one_pinned_az_is_rejected" {
  command = plan

  variables {
    availability_zones = ["ap-northeast-1a"]
  }

  expect_failures = [var.availability_zones]
}

# ---------------------------------------------------------------------------
# modules/network 単体
# ---------------------------------------------------------------------------

# /20 の VPC なら、サブネットは /28 (AWS の下限) で収まる。AZ 1 つも module としては通す。
run "module_accepts_slash_20" {
  command = plan

  module {
    source = "./modules/network"
  }

  variables {
    name               = "test"
    vpc_cidr           = "10.0.0.0/20"
    availability_zones = ["ap-northeast-1a"]
  }

  assert {
    condition     = aws_subnet.private[0].cidr_block == "10.0.0.160/28"
    error_message = "プライベートが 10 番目の /28 になっていない: ${aws_subnet.private[0].cidr_block}"
  }
}

# /24 の VPC を 8 ビット切ると /32 になり、AWS が受け付けない。
run "module_rejects_small_vpc" {
  command = plan

  module {
    source = "./modules/network"
  }

  variables {
    name               = "test"
    vpc_cidr           = "10.0.0.0/24"
    availability_zones = ["ap-northeast-1a", "ap-northeast-1c"]
  }

  expect_failures = [var.vpc_cidr]
}

# プライベートは 10 番からなので、AZ が 11 個だと public[10] と private[0] が重なる。
run "module_rejects_eleven_azs" {
  command = plan

  module {
    source = "./modules/network"
  }

  variables {
    name               = "test"
    vpc_cidr           = "10.0.0.0/16"
    availability_zones = ["a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"]
  }

  expect_failures = [var.availability_zones]
}
