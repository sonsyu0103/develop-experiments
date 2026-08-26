variable "project" {
  description = "リソース名の接頭辞。1 つの AWS アカウントに複数立てられるようにする"
  type        = string
  default     = "bbs"
}

variable "region" {
  description = "リソースを置くリージョン"
  type        = string
  default     = "ap-northeast-1"
}

variable "vpc_cidr" {
  description = "VPC の CIDR"
  type        = string
  default     = "10.0.0.0/16"
}

# **AZ は 2 つ要る。** ALB は最低 2 つの AZ にサブネットを持つ必要がある。
# 冗長性のためではなく、ALB の制約として要る。
variable "az_count" {
  description = "使うアベイラビリティゾーンの数"
  type        = number
  default     = 2

  validation {
    condition     = var.az_count >= 2
    error_message = "ALB が最低 2 AZ を要求するため、2 以上にしてください。"
  }
}

variable "db_instance_class" {
  description = "RDS のインスタンスクラス"
  type        = string
  default     = "db.t4g.micro"
}

variable "db_allocated_storage" {
  description = "RDS のストレージ (GiB)。gp3 の最小は 20"
  type        = number
  default     = 20
}

# **Spot を既定にする。**
# 中断されうるが、この用途 (面接の前に立てて見せる) では
# 落ちても取り返しがつく。通常の Fargate との差は約 70%。
# 本番では Spot と On-Demand を混ぜる (capacity_provider_strategy を分ける)。
variable "use_fargate_spot" {
  description = "Fargate Spot を使うか。false にすると通常の Fargate"
  type        = bool
  default     = true
}

variable "api_image_tag" {
  description = "デプロイする go-api のイメージタグ"
  type        = string
  default     = "latest"
}

variable "web_image_tag" {
  description = "デプロイする next-app のイメージタグ"
  type        = string
  default     = "latest"
}

# **GitHub Actions から OIDC で入るための設定。**
# アクセスキーを GitHub Secrets に置かないために要る (iam.tf)。
variable "github_repository" {
  description = "OIDC を許可する GitHub リポジトリ (owner/repo)"
  type        = string
  default     = "sonsyu0103/develop-experiments"
}

# **OIDC プロバイダは AWS アカウントに 1 つしか作れない。**
# 他のプロジェクトで既に作っている場合は false にして、既存を参照する。
variable "create_github_oidc_provider" {
  description = "GitHub の OIDC プロバイダを作るか。既に存在するなら false"
  type        = bool
  default     = true
}

# 認証は任意 (ADR 0005)。未設定でも API は起動し、
# ログインの 2 経路だけが 503 を返す。
variable "google_client_id" {
  description = "Google OIDC のクライアント ID。空ならログインだけ無効"
  type        = string
  default     = ""
}

variable "google_client_secret" {
  description = "Google OIDC のクライアントシークレット"
  type        = string
  default     = ""
  sensitive   = true
}

variable "alarm_email" {
  description = "アラームの通知先。空なら購読しない (SNS トピックは作る)"
  type        = string
  default     = ""
}

# **消し忘れを金額で見張る。** 予算アラート自体は無料。
variable "monthly_budget_usd" {
  description = "月額予算 (USD)。80% と予測 100% で通知する"
  type        = string
  default     = "20"
}
