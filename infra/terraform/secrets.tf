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
#
# ── **`nonsensitive()` で包む理由** ────────────────────────────
#
# `var.google_client_secret` は sensitive なので、**比較の結果まで
# sensitive として伝播する。** そのまま三項演算子に通すと、
# 分岐の結果を含む式が丸ごと機密扱いになり、
# **ecs.tf の `container_definitions` が毎回 `(sensitive value)` に潰れる。**
# image tag や CORS_ALLOWED_ORIGINS を変えても plan で差分が読めなくなる。
#
# 最小構成で再現を確認した (Terraform 1.15.8):
#
#   locals { e = var.s != "" }          # var.s は sensitive
#   output "x" { value = e ? "A" : "B" }
#   -> Error: Output refers to sensitive values
#
# ここで漏れるのは**「設定されているか」の真偽値だけ**で、値そのものは
# 通さない。真偽は `aws_secretsmanager_secret.google_oauth` が
# plan に出るかどうかで既に分かるので、隠す意味がない。
locals {
  google_oauth_enabled = nonsensitive(var.google_client_secret != "")
}

resource "aws_secretsmanager_secret" "google_oauth" {
  count = local.google_oauth_enabled ? 1 : 0

  name = "${local.name}/google-oauth"

  # **0 にする理由は rds.tf の db secret と同じ。**
  # 既定 (30 日) だと destroy 後も論理削除で残り、同じ名前で
  # 作り直せない —— 「使う日だけ立てる」運用と噛み合わない。
  #
  # ── **この値の複製は tfvars しか無い** ─────────────────────
  #
  # `_wo` にしたことで state から値が消えたので、AWS の外にある唯一の
  # 複製は **gitignore 済みの `terraform.tfvars`** だけになった。
  #
  # つまり **tfvars を持たない端末や経路で apply すると、
  # `count = 0` に倒れて secret が即時完全に削除される** ——
  # `recovery_window_in_days = 0` なので復旧の猶予も無く、
  # 非対話 apply では "1 to destroy" が流れていく。
  # 症状はログインが黙って 503 に戻ることだけになる。
  #
  # **`prevent_destroy` は付けない。** 付けると `terraform destroy` が
  # 止まり、「消せること」を要件にした構成が壊れる (ADR 0024 決定 1) ——
  # 消し忘れの課金のほうが痛い。
  # 代わりに variables.tf の validation で「片方だけ設定」を弾き、
  # 打ち間違いによる意図しない無効化は潰してある。
  # **tfvars 自体のバックアップは運用の責任**として ADR に残す。
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
  secret_string_wo = jsonencode({ client_secret = var.google_client_secret })

  # ── **版番号は値から導出する。手で上げない** ───────────────────
  #
  # ここを固定値にすると、tfvars の値を差し替えても
  # **`plan` が `No changes` を返す** (実測)。差分が 0 件なので
  # apply する機会すら来ず、**ローテーションが静かに失敗する。**
  # AWS 側は古い値のまま、手元の tfvars だけが新しくなる。
  #
  # 値のハッシュから作れば、値が変われば番号も変わる ——
  # 人の手順に依存しなくなる。
  #
  # **単調増加しなくてよい。** この番号を見るのは Terraform だけで
  # (AWS には送られない)、変化したかどうかしか使われない。
  # 上がろうが下がろうが、新しい版が書かれる。
  #
  # **`nonsensitive()` は使わない。** ハッシュの先頭 32bit でも、
  # 値を推測した人がオフラインで照合できてしまう。
  # 機密のまま置くと plan では `(sensitive value)` と出るだけで、
  # 「変わったこと」は差分の有無で分かるので運用に困らない。
  secret_string_wo_version = parseint(substr(sha256(var.google_client_secret), 0, 8), 16)
}
