# アプリに渡す秘密のうち、DB 以外。
# (DB の接続情報は rds.tf 側 —— パスワードを生成する主体が RDS なので、
#  そちらに置いたほうが「生成 → 保管 → 注入」が 1 ファイルで追える。)
#
# ── **environment に書いてはいけない** ──────────────────────
#
# ECS のタスク定義は `ecs:DescribeTaskDefinition` があれば読める。
# `environment` に入れた値はその応答に平文で出るので、
# **タスク定義を読める人全員に見える。**
# 秘密は `secrets` で渡し、実行ロールに Secrets Manager を読ませる。
#
# 対象を分けている理由:
#   GOOGLE_CLIENT_ID     秘密でない。認可のリダイレクト URL に載って
#                        ブラウザまで出るので、environment のまま
#   GOOGLE_CLIENT_SECRET 秘密。ここで Secrets Manager に入れる

# **認証は任意** (ADR 0005)。未設定なら secret ごと作らない。
#
# 空文字で作ろうとしても Secrets Manager が受け付けない
# (SecretString は 1 文字以上) ので、count で分岐させる。
#
# アプリ側は os.Getenv で読んでいる (internal/config/config.go:605) ため、
# **「環境変数が無い」と「空文字」は同じ挙動**になる ——
# AuthConfig.Enabled() が false を返し、ログインの 2 経路だけが 503。
# environment から外しても、無効時の振る舞いは変わらない。
locals {
  google_oauth_enabled = var.google_client_secret != ""
}

resource "aws_secretsmanager_secret" "google_oauth" {
  count = local.google_oauth_enabled ? 1 : 0

  name = "${local.name}/google-oauth"

  # **0 にする理由は rds.tf の db secret と同じ。**
  # 既定 (30 日) だと destroy 後も論理削除で残り、同じ名前で
  # 作り直せない —— 「使う日だけ立てる」運用と噛み合わない。
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "google_oauth" {
  count = local.google_oauth_enabled ? 1 : 0

  secret_id = aws_secretsmanager_secret.google_oauth[0].id

  # db secret と同じく JSON にする。ECS の valueFrom は
  # `<arn>:<json-key>::` でキーを 1 つ取り出せるので、
  # 後で別の値 (SMTP のパスワードなど) を足しても注入側を変えずに済む。
  #
  # ── **`_wo` (write-only) を使う。値を state に残さないため** ──────
  #
  # 素の `secret_string` は state に平文で入る。`secret_string_wo` は
  # apply のときだけプロバイダに渡され、**state にも plan にも残らない**
  # (Terraform 1.11 以降。versions.tf の required_version を参照)。
  #
  # **代償: Terraform は中身の変化を検知できない。**
  # 値が state に無いので差分の取りようがなく、更新は
  # `secret_string_wo_version` を**人が上げたときだけ**起きる。
  #
  # ここでは成立する —— 中身が var 1 個で、他リソースの属性に依存しない。
  # **db secret には使わない。** あちらは host / endpoint を
  # `aws_db_instance` から組み立てるので、追跡されないと
  # 「RDS が置き換わったのに secret は古いホストを指したまま」になる
  # (docs/adr/0024-aws-deployment.md の「秘密の渡し方」)。
  #
  # **ローテーションの手順**: tfvars の値を入れ替え、下の番号を +1 して apply。
  # 番号を上げ忘れると、**apply は成功するのに値が変わらない。**
  secret_string_wo         = jsonencode({ client_secret = var.google_client_secret })
  secret_string_wo_version = 1
}
