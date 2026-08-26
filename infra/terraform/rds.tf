# PostgreSQL (ADR 0001)。
#
# ── **destroy できることを要件にする** ─────────────────────
#
# 「必要な日だけ立てる」運用なので、消せないリソースが 1 つでもあると
# terraform destroy が途中で止まり、**残骸が課金され続ける。**
# RDS は既定がその逆 (消えにくいほう) に倒れているので、明示的に外す:
#
#   skip_final_snapshot     最終スナップショットを作らない
#   deletion_protection     削除保護を外す
#   backup_retention_period 0 (自動バックアップを取らない)
#
# **本番ではすべて逆にする。** その対比は
# docs/adr/0024-aws-deployment.md に表で残す。

resource "aws_db_subnet_group" "main" {
  name       = local.name
  subnet_ids = aws_subnet.private[*].id

  tags = { Name = local.name }
}

# **パスワードは Terraform に作らせる。**
# tfvars に手で書くと、コミットしない運用を人間が守り続けることになる。
# 生成した値は state に平文で入るので、state 自体を秘密として扱う
# (.gitignore 済み。共有するなら S3 backend + 暗号化)。
resource "random_password" "db" {
  length = 32

  # **RDS が受け付けない文字を外す。**
  # `/`, `@`, `"`, スペースは master password に使えない。
  # ここを忘れると apply の最後で失敗する。
  override_special = "!#$%&*()-_=+[]{}<>:?"
}

resource "aws_secretsmanager_secret" "db" {
  name = "${local.name}/db"

  # **0 にする。** 既定 (30 日) だと destroy しても論理削除で残り、
  # **同じ名前で作り直せない** —— 立て直しのたびに
  # "already scheduled for deletion" で apply が失敗する。
  # 「使う日だけ立てる」運用と、既定の復旧猶予は相性が悪い。
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "db" {
  secret_id = aws_secretsmanager_secret.db.id

  # go-api は DATABASE_URL 1 本で受け取る (config.go)。
  # 分解した値も入れておくと、psql で入るときに使える。
  secret_string = jsonencode({
    username = local.db_username
    password = random_password.db.result
    host     = aws_db_instance.main.address
    port     = aws_db_instance.main.port
    dbname   = local.db_name
    url      = "postgres://${local.db_username}:${urlencode(random_password.db.result)}@${aws_db_instance.main.endpoint}/${local.db_name}?sslmode=require"
  })
}

locals {
  db_username = "app"
  db_name     = "bbs"
}

resource "aws_db_instance" "main" {
  identifier = local.name

  engine = "postgres"
  # メジャーバージョンだけ固定する。マイナーは自動更新に任せる
  # (ローカルは postgres:17-alpine)。
  engine_version             = "17"
  auto_minor_version_upgrade = true

  instance_class    = var.db_instance_class
  allocated_storage = var.db_allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = local.db_name
  username = local.db_username
  password = random_password.db.result

  db_subnet_group_name   = aws_db_subnet_group.main.name
  vpc_security_group_ids = [aws_security_group.rds.id]

  # **プライベートサブネットに置き、外からは触れない。**
  # 手元から psql でつなぐときは ECS Exec 経由か踏み台を立てる。
  publicly_accessible = false

  # Single-AZ。**本番なら Multi-AZ** (ADR 0009 のインフラ構成図)。
  # ポートフォリオ用の構成では、待機系に月 $15 を払う理由がない。
  multi_az = false

  # ---- ここから下は「消せること」のための設定 ----
  skip_final_snapshot     = true
  deletion_protection     = false
  backup_retention_period = 0

  # Performance Insights は無料枠 (7 日) があるが、
  # 立てて壊してを繰り返す用途では読む機会が無いので切る。
  performance_insights_enabled = false

  # **ロケールに注意。**
  # ローカルの compose は LC_COLLATE/LC_CTYPE=C で initdb している
  # (比較を速くするため)。RDS では initdb の引数を渡せないので
  # 既定の en_US.UTF-8 になる。
  #
  # **この差は pg_trgm の索引に効く** (ADR 0012)。
  # C ロケールだと日本語からトライグラムを作れないが、
  # RDS 側はその制限を受けない —— つまり
  # **手元のほうが厳しい条件**で、本番で急に壊れる向きではない。
  # 逆にソート順は手元と RDS で変わりうる。

  tags = { Name = local.name }
}
