# Terraform とプロバイダのバージョンを固定する。
#
# **`~>` の上限を開けない** —— Makefile 冒頭でツールを固定しているのと
# 同じ理由になる。ある日プロバイダにルールが増えて、
# 触っていない構成の apply が突然落ちるのを避ける。
# 更新は「意図した変更」としてコミットに残す。

terraform {
  # 1.10 以降を要求する。**S3 backend のロックが DynamoDB 無しで効く**
  # バージョンで、backend を移すときに作るものが 1 つ減る (下の backend の節)。
  required_version = "~> 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }

    # DB のパスワードを生成する。**手で決めた値を tfvars に置かない** ——
    # 置いた瞬間、コミットしない運用を人間が守り続ける必要が出る。
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # **backend は local のままにする。**
  #
  # 実務なら S3 backend を使う。ここで使わないのは、
  # **state の置き場そのものが destroy できないリソースになる**ため:
  #
  #   - state 用の S3 バケットを同じ構成で作ると、destroy の途中で
  #     自分の state を消すことになる (鶏と卵)
  #   - 別 stack に切り出すと、「使う日だけ立てる」運用のたびに
  #     2 つの stack を面倒みることになる
  #
  # この構成は **apply と destroy を繰り返すことを前提にしている** ので、
  # state が手元にあるほうが素直になる。tfstate は .gitignore 済み。
  #
  # チームで共有するなら、この block をこう置き換える:
  #
  #   backend "s3" {
  #     bucket       = "<事前に手で作ったバケット>"
  #     key          = "bbs/terraform.tfstate"
  #     region       = "ap-northeast-1"
  #     encrypt      = true
  #     use_lockfile = true   # 1.10 以降。DynamoDB のロックテーブルは要らない
  #   }
}

provider "aws" {
  region = var.region

  # **すべてのリソースに同じタグを付ける。**
  # 「使う日だけ立てる」運用では、消し忘れたリソースを
  # タグで洗い出せることが命綱になる (コスト事故はここから起きる)。
  default_tags {
    tags = {
      Project   = var.project
      ManagedBy = "terraform"
      Repo      = "develop-experiments"
    }
  }
}

# CloudFront が使う証明書は **us-east-1 にしか置けない。**
# いまは CloudFront の既定ドメイン (*.cloudfront.net) を使うので
# 証明書を作らないが、独自ドメインに移す日に必要になるため
# エイリアスだけ先に用意しておく。
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  default_tags {
    tags = {
      Project   = var.project
      ManagedBy = "terraform"
      Repo      = "develop-experiments"
    }
  }
}
