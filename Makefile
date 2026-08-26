.DEFAULT_GOAL := help

GO_API_DIR  := apps/go-api
NEXT_DIR    := apps/next-app

# Node を叩くターゲットは NODE_OPTIONS を外して実行する。
#
# 親シェルに NODE_OPTIONS=--require=... が入っていると、そのモジュールを
# 解決できない環境で node が起動時に落ちる。**Makefile の中身とは無関係な
# 理由で make が失敗する**ため、原因が分かりにくい。
#
# 呼ぶ側に env -u NODE_OPTIONS を付けて回避していたが、
# 回避策を文書に書いた時点で直す場所が違う。ここで閉じる。
NODE := env -u NODE_OPTIONS

# migrate コンテナは compose ネットワーク内から接続するため、ホスト名はサービス名
MIGRATE_URL := postgres://app:password@postgres:5432/bbs?sslmode=disable

# MinIO を mc で操作する使い捨てコンテナ。
#
# **MC_HOST_<alias> で接続先を渡す。** `mc alias set` を先に流す形だと
# entrypoint を sh にする必要があり、minio/mc のイメージには
# grep すら無いのでシェル芸が書けない (実測)。環境変数なら 1 コマンドで済む。
MC := docker compose run --rm \
	-e MC_HOST_local=http://minioadmin:minioadmin@minio:9000 \
	--entrypoint mc minio-init

# バージョンを固定する理由:
#   @latest のままだと、ある日リンタのルールが増えて CI が突然赤くなる。
#   更新は「意図した変更」としてコミットに残したい。
GOLANGCI_LINT_VERSION := v2.12.2
SQLC_VERSION          := v1.31.1
OAPI_CODEGEN_VERSION  := v2.8.0
GO_ARCH_LINT_VERSION  := v1.17.0

.PHONY: help
help: ## このヘルプを表示する
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-24s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# 環境の起動
# ---------------------------------------------------------------------------

.PHONY: up
up: ## 全サービスを起動する (マイグレーションも自動実行)
	docker compose up -d --build
	@$(MAKE) --no-print-directory verify-search-locale

.PHONY: verify-search-locale
verify-search-locale: ## pg_trgm が日本語を索引できるか確かめる (警告のみ)
	# **LC_CTYPE が C だと、pg_trgm は日本語からトライグラムを 1 つも作らない。**
	# 索引は作られ、CREATE INDEX も成功し、エラーもどこにも出ない ——
	# 現れるのは実行計画だけになる (docs/adr/0012-search.md)。
	#
	# Phase 11 より前に作ったボリュームがこの状態になる。
	# **initdb の引数なので、作り直さないと直らない。**
	#
	# 【落とさない理由】
	# 検索の結果自体は正しい (一致判定は索引と無関係で、変わるのは速度だけ)。
	# ここで make up を失敗させると、検索を触らない作業まで止まる。
	# CI 側は同じ検査で**落とす** —— あちらは毎回新しいコンテナなので、
	# 空になったら設定が壊れたことを意味する。
	@n=$$(docker compose exec -T postgres psql -U app -d bbs -tAc \
		"SELECT coalesce(array_length(show_trgm('検証'), 1), 0)" 2>/dev/null \
		| tr -d '[:space:]'); \
	if [ -n "$$n" ] && [ "$$n" -lt 1 ] 2>/dev/null; then \
		echo ""; \
		echo "警告: このデータベースでは pg_trgm が日本語を索引できません (LC_CTYPE=C)。"; \
		echo "      検索は動きますが GIN 索引が使われず、全表走査になります。"; \
		echo "      直すにはボリュームの作り直しが要ります (データは消えます):"; \
		echo ""; \
		echo "          make clean && make up && make seed"; \
		echo ""; \
		echo "      詳細: docs/adr/0012-search.md の「実装して分かったこと 1」"; \
		echo ""; \
	fi

.PHONY: up-seeded
up-seeded: up ## 起動したうえでシードデータまで投入する (既存データは消える)
	# up と分けているのは、seed.sql が TRUNCATE から始まるため。
	# make up にまとめると、起動のたびに手入力のデータが消える。
	@$(MAKE) --no-print-directory seed

# ---------------------------------------------------------------------------
# エッジ (TLS 終端) —— ローカルの擬似 AWS
# ---------------------------------------------------------------------------

# エッジが名乗るホスト名。**既定は localhost。**
# bbs.localhost のような .localhost サブドメインを既定にすると、
# ブラウザでは見えるのに curl だけ名前解決に失敗する環境がある。
SITE_ADDRESS ?= localhost

# X-Forwarded-For を信じる送信元。compose の既定ネットワークは
# 172.16.0.0/12 の中から動的に割り当てられるので、その範囲を丸ごと信じる。
# **本番でこの書き方はしない** —— ALB が居るサブネットだけを指定する。
TRUSTED_PROXIES ?= 172.16.0.0/12

# エッジ経由の構成に切り替えるための環境変数一式。
#
# **単一オリジンにする** (https://localhost だけ)。本番構成の
# CloudFront が / と /api/* を 1 つのドメインに集約するのと同じ形で、
# こうすると CORS が本当に不要になり、csrfGuard は selfOrigin() の
# 一致だけで通る —— つまり X-Forwarded-Proto が唯一の生命線になる。
HTTPS_ENV := \
	SITE_ADDRESS=$(SITE_ADDRESS) \
	CORS_ALLOWED_ORIGINS=https://$(SITE_ADDRESS) \
	AUTH_REDIRECT_URL=https://$(SITE_ADDRESS)/api/auth/google/callback \
	AUTH_FRONTEND_URL=https://$(SITE_ADDRESS) \
	NEXT_PUBLIC_API_URL=https://$(SITE_ADDRESS)/api \
	S3_PUBLIC_BASE_URL=https://$(SITE_ADDRESS)/images \
	TRUSTED_PROXIES=$(TRUSTED_PROXIES) \
	SECURE_COOKIE=true

.PHONY: up-https
up-https: ## エッジ (TLS 終端) ごと起動する (https://localhost)
	# **ENV は development のまま。** ENV=production にすると
	# COMMENT_POST_MODE=naive などの実測用モードが選べなくなるため、
	# 本番同型を試したいときだけ明示する:
	#   ENV=production make up-https
	#
	# ただし **Cookie の Secure だけは ENV と切り離して常に付ける**
	# (SECURE_COOKIE=true)。エッジ経由は https なので付けられるし、
	# 付けないと開発環境だけが本番と違う Cookie を配ることになる。
	#
	# 証明書は Caddy 内蔵の CA が発行する。ブラウザは初回に警告を出す ——
	# 消したければ make trust-ca。
	$(HTTPS_ENV) docker compose --profile edge up -d --build
	@$(MAKE) --no-print-directory verify-search-locale
	@echo
	@echo "  https://$(SITE_ADDRESS)        画面"
	@echo "  https://$(SITE_ADDRESS)/api/   API (プレフィックスは剥がされる)"
	@echo "  https://$(SITE_ADDRESS)/images/ 画像 (MinIO へ直結)"
	@echo
	@echo "  従来の http://localhost:3000 / :8080 もそのまま生きている。"

.PHONY: trust-ca
trust-ca: ## エッジの CA 証明書を取り出す (ブラウザの警告を消したいとき)
	# Caddy が発行したローカル CA のルート証明書を取り出す。
	# **この CA はこの環境専用**で、caddy-data ボリュームを消すと変わる。
	@mkdir -p $(dir $(CADDY_ROOT_CA))
	@docker compose --profile edge exec -T edge \
		cat /data/caddy/pki/authorities/local/root.crt > $(CADDY_ROOT_CA)
	@echo "取り出した: $(CADDY_ROOT_CA)"
	@echo
	@echo "macOS のキーチェーンに入れる (管理者パスワードを聞かれる):"
	@echo "  sudo security add-trusted-cert -d -r trustRoot \\"
	@echo "    -k /Library/Keychains/System.keychain $(CADDY_ROOT_CA)"
	@echo
	@echo "戻すとき (CN は発行年を含むので、実際の値から引く):"
	@echo "  sudo security delete-certificate -c \"$$(openssl x509 -in $(CADDY_ROOT_CA) -noout -subject | sed 's/.*CN=//')\""

# 取り出した CA の置き場。**リポジトリの外に出す。**
# 中身は「この環境だけを信頼させる鍵」なので、コミットする理由がない。
CADDY_ROOT_CA ?= $(HOME)/claude-artifacts/caddy-local-root.crt

.PHONY: https-verify
https-verify: ## エッジ配下でしか通らない分岐を実測する (XFP を壊して効きも見る)
	# **プロキシの背後でしか通らない分岐を、実際に通して確かめる。**
	#
	# ADR 0013 (HTTP 防御) と ADR 0005 (Cookie) は「本番ならこう振る舞う」
	# という推論のうえに書かれていた。compose が全部 http だったため、
	# X-Forwarded-Proto / Secure / TRUSTED_PROXIES はどれも一度も
	# 実行されていない。
	#
	# **検査の効きも見る。** XFP をわざと落として書き込みが 403 になることを
	# 確認する —— 壊しても通るなら、この検査は何も守っていない
	# (LOG_FORMAT=text make logs-verify と同じ考え方)。
	#
	# 検査で作ったスクラッチのスレッドは最後に消す。
	@$(MAKE) --no-print-directory up-https
	# **HTTPS_ENV をスクリプトにも渡す。** 渡さないと、スクリプト内の
	# docker compose が既定値 (http) で go-api を作り直し、
	# その起動待ちに出る 502 を検査結果と取り違える。
	@$(HTTPS_ENV) bash .github/scripts/https-probe.sh

# ---------------------------------------------------------------------------
# AWS (Terraform) —— 必要な日だけ立てる
# ---------------------------------------------------------------------------
#
# **常時公開しない運用にしている。**
# 立てっぱなしにすると月 $40 前後かかる。見せたい日に apply して、
# 終わったら destroy する。1 日あたり $1.3 ほど。
#
# そのために「消せること」を構成の要件にしてある
# (docs/adr/0024-aws-deployment.md)。RDS の最終スナップショットや
# S3 の中身が destroy を止めないよう、既定を明示的に外している。

TF_DIR := infra/terraform
TF     := terraform -chdir=$(TF_DIR)

# リソース名と Project タグの接頭辞。**infra/terraform/variables.tf の
# 既定と揃える。** destroy 後は state から output が消えるので、
# 確認手順はこちらを使う (terraform に問い合わせない)。
PROJECT ?= bbs

.PHONY: tf-init
tf-init: ## Terraform を初期化する (最初の 1 回)
	$(TF) init

.PHONY: tf-fmt
tf-fmt: ## Terraform の書式を揃える
	$(TF) fmt -recursive

.PHONY: tf-validate
tf-validate: ## Terraform の構成を検査する (AWS に触らない)
	$(TF) validate

.PHONY: tf-plan
tf-plan: ## 何が作られるかを見る (課金は発生しない)
	$(TF) plan

# apply / destroy の計画を固めるファイル。**リポジトリに置かない。**
TF_PLAN ?= $(HOME)/claude-artifacts/bbs-apply.tfplan

.PHONY: tf-apply
tf-apply: ## AWS に環境を作る (**課金が発生する**)
	# **plan をファイルに固めてから適用する** (tf-destroy と同じ形)。
	#
	# 素の `terraform apply` は対話で確認を求めるため、端末の無い経路
	# (CI、エディタからの実行) では**プロンプトで止まる**。
	# apply 側で止まると、RDS や ALB が中途半端に作られたまま
	# 放置されうるので、destroy 側より始末が悪い。
	#
	# 何が作られるかは下の plan の出力に全部出る。
	# RDS の作成に 5〜10 分かかる。全体で 15 分ほど見ておく。
	@mkdir -p $(dir $(TF_PLAN))
	$(TF) plan -out=$(TF_PLAN)
	$(TF) apply $(TF_PLAN)
	@rm -f $(TF_PLAN)
	@echo
	@echo "次にやること:"
	@echo "  make tf-push     イメージを ECR へ送る"
	@echo "  make tf-migrate  マイグレーションを流す"
	@echo "  make tf-url      公開 URL を見る"

# destroy の計画を固めるファイル。**リポジトリに置かない。**
TF_DESTROY_PLAN ?= $(HOME)/claude-artifacts/bbs-destroy.tfplan

.PHONY: tf-destroy
tf-destroy: ## AWS の環境を消す (**消し忘れると課金され続ける**)
	# **plan をファイルに固めてから適用する。**
	#
	# 素の `terraform destroy` は対話で確認を求めるため、
	# 端末の無い環境 (Claude Code の ! 実行など) では
	# **プロンプトで止まったまま何も消えない** —— 消したつもりで
	# 課金が続く、といういちばん避けたい失敗になる。
	#
	# plan を挟めば「何が消えるか」は下に全部出るし、
	# 適用は非対話で完了する。
	@mkdir -p $(dir $(TF_DESTROY_PLAN))
	$(TF) plan -destroy -out=$(TF_DESTROY_PLAN)
	$(TF) apply $(TF_DESTROY_PLAN)
	@rm -f $(TF_DESTROY_PLAN)
	@echo
	@echo "消えたことを確かめる:"
	# **terraform output から取らない。** destroy が終わると state ごと
	# output も消えるので、必ずフォールバック側に落ちる ——
	# project を変えた環境では存在しないタグを引き、
	# **0 件と表示されて「消えている」と誤読する。**
	# 消し忘れを確かめるための手順が、いちばん危ない場面で嘘をつく。
	#
	# SITE_ADDRESS から導出するのも同じ理由で誤り (あれはローカルの
	# エッジのホスト名で、AWS の Project タグとは無関係)。
	@echo "  aws resourcegroupstaggingapi get-resources --tag-filters Key=Project,Values=$(PROJECT) --query 'length(ResourceTagMappingList)'"


.PHONY: tf-push
tf-push: ## イメージをビルドして ECR へ push する (ARM64)
	@bash infra/scripts/push-images.sh

.PHONY: tf-migrate
tf-migrate: ## マイグレーションを AWS 上で 1 回流す (make tf-migrate ARGS="force 9")
	# **失敗して dirty になったら force で解除する。**
	# golang-migrate は途中で失敗するとフラグを立て、
	# 人間が確認するまで再開しない (安全装置)。
	#   make tf-migrate ARGS="force 9"   直前の版まで戻して解除
	#   make tf-migrate                  up (既定)
	@bash infra/scripts/run-migrate.sh $(ARGS)

.PHONY: tf-deploy
tf-deploy: ## push してサービスを入れ替える (安定するまで待つ)
	@bash infra/scripts/deploy.sh

.PHONY: tf-url
tf-url: ## 公開 URL を表示する
	@$(TF) output -raw public_url && echo

.PHONY: tf-github-vars
tf-github-vars: ## CD が使う値を GitHub の Variables に登録する
	# **terraform output から取る。** 立て直すと CloudFront の
	# ドメインが変わるので、手で貼り直すと必ず古い値が残る。
	#
	# 入るのはロール ARN やクラスタ名だけで、資格情報は 1 つも置かない
	# (OIDC。infra/terraform/iam.tf)。
	@bash infra/scripts/setup-github-vars.sh

.PHONY: tf-verify
tf-verify: ## apply した AWS 環境を実測する (手元の https-verify の AWS 版)
	# **ADR 0024 の「まだ確かめていないこと」を潰す。**
	#
	# とくに見たいのは 2 つ:
	#   - CloudFront Function がパスを剥がせているか
	#   - **書き込みが 201 になるか** —— ALB が X-Forwarded-Proto を
	#     http で上書きするため、CORS_ALLOWED_ORIGINS の明示だけが
	#     書き込みを守っている (ADR 0023 の 3 で手元に再現した形)
	#
	# ALB を直接叩いて届かないこと (CloudFront を迂回できないこと) も見る。
	@bash infra/scripts/verify.sh

.PHONY: tf-secret
tf-secret: ## DB の接続情報を表示する (Secrets Manager から読む)
	# **RDS はプライベートサブネットに居るので、手元からは直接つなげない。**
	# go-api は distroless なので ECS Exec でも入れない (shell が無い)。
	# つなぐ必要が出たら、next-app のタスク (alpine) に入るか、
	# 一時的な psql タスクを run-task で立てる。
	@aws secretsmanager get-secret-value \
		--region $$($(TF) output -raw region) \
		--secret-id $$($(TF) output -raw db_secret_arn) \
		--query SecretString --output text | python3 -m json.tool

.PHONY: down
down: ## 全サービスを停止する (データは残る)
	# **--profile edge を付ける。** profile 付きのサービスは
	# 素の down では止まらず、エッジだけが 80 / 443 を握ったまま残る。
	# しかも upstream が全部落ちた状態なので、
	# https://localhost は 502 を返し続ける —— 止めたつもりで止まっていない。
	docker compose --profile edge down

.PHONY: clean
clean: ## 全サービスを停止し、DB のデータも消す
	# caddy-data も消えるので、次の up-https では CA が作り直される。
	# make trust-ca でキーチェーンに入れていた場合は入れ直しになる。
	docker compose --profile edge down -v

.PHONY: logs
logs: ## ログを追尾する
	# **go-api のログは fluent-bit 側に出る** (ADR 0010 決定 1)。
	# fluentd ログドライバが stdout を横取りするので、
	# `docker compose logs go-api` は空になる。
	# 全サービスをまとめて追尾するこの形なら、どちらでも見える。
	docker compose logs -f

.PHONY: logs-query
logs-query: ## S3 に着地したログを SQL で読む (make logs-query Q='SELECT ...')
ifndef Q
	$(error Q が未指定です。例: make logs-query Q='SELECT msg, count(*) FROM go_api_logs GROUP BY 1')
endif
	# ビュー名は go_api_logs。パーティション列 (service / dt / hour) も見える。
	# 保存したクエリは /queries に読み取り専用でマウントしてある:
	#   make logs-query Q=/queries/http-status.sql
	docker compose run --rm logs-query "$(Q)"

.PHONY: logs-verify
logs-verify: ## ログ基盤が本番と同じ形で動いているかを実測する
	# **目視では気づけない壊れ方を捕まえる。**
	# パーサが展開に失敗しても fluent-bit はレコードを捨てないので、
	# S3 にはオブジェクトが増え続ける —— 見た目には動いている。
	# 気づくのは Athena でクエリを書こうとした数日後になる。
	#
	# 実際にその形で壊れていた (ADR 0010「実装して分かったこと 1」)。
	@$(MAKE) --no-print-directory up
	# **過去のログを消してから測る。** 混ざると、いま壊れている設定でも
	# 「前に着地した正しいログ」で検査が通ってしまう。
	@echo "S3 上の既存ログを消しています..."
	@$(MC) rm --recursive --force local/bbs-logs/logs/ >/dev/null 2>&1 || true
	# go-api を作り直す。LOG_FORMAT を変えた直後に、
	# 古い設定のまま動いているプロセスを検査しないようにする (make smoke と同じ理由)。
	docker compose up -d --force-recreate go-api
	@for i in $$(seq 1 45); do \
		curl -sf -o /dev/null http://localhost:8080/healthz && break || sleep 2; \
	done
	# **いろいろな応答を出させる。** 200 だけだと、ステータスや
	# レイテンシで切るクエリが「動いたが何も分からない」形になる。
	@curl -sf -o /dev/null "http://localhost:8080/threads" || true
	@curl -sf -o /dev/null "http://localhost:8080/threads?size=1" || true
	@curl -s  -o /dev/null "http://localhost:8080/threads/999999" || true
	@curl -s  -o /dev/null "http://localhost:8080/threads?cursor=壊れたトークン" || true
	@curl -s  -o /dev/null -X POST "http://localhost:8080/threads" \
		-H 'Content-Type: application/json' -d '{"title":""}' || true
	# **スレッド詳細も叩く。** 閲覧が計上され、フラッシュが走ると
	# view_count_flushed が着地する。
	#
	# レビュー指摘で分かったことだが、**この検査は「出たログ」しか見られない**。
	# 叩かない経路のイベントは、DDL に宣言し忘れていても検出されない。
	# 網を完全にはできないので、**少なくとも今回入れたイベントは通す。**
	@curl -sf -o /dev/null "http://localhost:8080/threads/1" || true
	@curl -sf -o /dev/null "http://localhost:8080/threads/2" || true
	# **待ち合わせは検査側が持っている。** ここで「オブジェクトが 1 つ
	# できたか」を見て進むと、アプリの起動ログ (gin のルート登録) が
	# 先にアップロードされた時点で抜けてしまい、**リクエストのログが
	# 1 件も無い状態で検査を始める**ことになる (実測で誤検出した)。
	# verify.py は http_request が着地するまで待つ。
	docker compose run --rm --entrypoint python logs-query /opt/logs/verify.py

.PHONY: psql
psql: ## PostgreSQL に対話接続する
	docker compose exec postgres psql -U app -d bbs

# ---------------------------------------------------------------------------
# マイグレーション
# ---------------------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## マイグレーションを最新まで適用する
	docker compose run --rm migrate -path=/migrations -database="$(MIGRATE_URL)" up

.PHONY: migrate-down
migrate-down: ## マイグレーションを 1 つ巻き戻す
	docker compose run --rm migrate -path=/migrations -database="$(MIGRATE_URL)" down 1

.PHONY: migrate-create
migrate-create: ## 新しいマイグレーションを作る (make migrate-create NAME=add_foo)
ifndef NAME
	$(error NAME が未指定です。例: make migrate-create NAME=add_user_table)
endif
	docker compose run --rm migrate -path=/migrations create -ext sql -dir /migrations -seq $(NAME)

.PHONY: seed
seed: ## 開発用のシードデータを投入する (既存データは消える)
	# compose の seed サービスとして定義してある。
	# psql の起動条件 (ON_ERROR_STOP、マイグレーション完了待ち) を
	# compose.yaml とこことで二重に持たないため、run に寄せている。
	# 未起動なら postgres と migrate は depends_on 経由で立ち上がる。
	docker compose run --rm seed

# ---------------------------------------------------------------------------
# コード生成
# ---------------------------------------------------------------------------
# 発生源は 2 つだけ:
#   apps/go-api/db/query/*.sql  → Go の型付きクエリ (sqlc)
#   api/openapi.yaml            → Go のサーバスタブ (oapi-codegen) と TS の型
# ---------------------------------------------------------------------------

.PHONY: sqlc
sqlc: ## SQL から Go のコードを生成する
	cd $(GO_API_DIR) && sqlc generate

.PHONY: openapi
openapi: ## openapi.yaml から Go スタブと TypeScript 型を生成する
	cd $(GO_API_DIR) && oapi-codegen -config oapi-codegen.yaml ../../api/openapi.yaml
	cd $(NEXT_DIR) && $(NODE) npm run gen:types

.PHONY: generate
generate: sqlc openapi ## 生成物をすべて作り直す

# ---------------------------------------------------------------------------
# 検証
# ---------------------------------------------------------------------------

.PHONY: test
test: ## Go のテストを実行する (race detector + カバレッジ。実 DB / S3 は使わない)
	# **実 DB と S3 を使う検査は、ここでは走らせない。**
	#
	# 環境変数を明示的に空にする。**残しておくと、手元に DB があるかどうかで
	# make check の結果が変わる** —— 実際、開発用シードを流したあとだと
	# 落ちる状態になっていた (ベンチ用データセットを前提にした検査があった)。
	# CI の Go Checks には DB が無いので、そちらでは元から走っていない。
	# **手元と CI で同じものを検査する**ほうを取る。
	#
	# 実 DB を使う検査は make test-live (と CI の Migration Check) が持つ。
	cd $(GO_API_DIR) && DATABASE_TEST_URL= S3_TEST_ENDPOINT= \
		DB_TEST_REQUIRE= S3_TEST_REQUIRE= \
		go test -race -cover ./...

# 実 DB / MinIO に対する検査。**環境が要るので check には入れない。**
LIVE_DATABASE_URL ?= postgres://app:password@localhost:5432/bbs?sslmode=disable
LIVE_S3_ENDPOINT  ?= http://localhost:9000

.PHONY: test-live
test-live: up ## 実 DB / MinIO に対する検査だけを走らせる (環境も立てる)
	# **CI の Migration Check と同じものを手元で回す。**
	#
	# 名前の規約は _Live で終わること。CI は -run '_Live$$' で拾うので、
	# **規約から外れた検査はどこでも走らなくなる** ——
	# 実際 TestNPlusOneMatchesSingleQuery が長らくその状態だった。
	#
	# REQUIRE を立てるので、URL が欠けていれば**スキップではなく失敗**する。
	#
	# 【up に依存させる理由 —— スタックが落ちていると Fatal で止まる】
	# DB_TEST_REQUIRE=1 を立てているので、接続できないときは
	# **スキップではなく Fatal** になる (レビュー指摘)。
	# 環境を立てるところまでこのターゲットが持つ。
	#
	# **up-seeded ではなく up にする。** up-seeded は seed.sql を流し、
	# その先頭が TRUNCATE なので**手元のデータを毎回消す。**
	# 検査は必要な行を自分で作るようにしてあるので (ensureSomeViewCount /
	# seedThreadForCompare / seedThreadTitled)、シードに依存する理由が無い。
	#
	# 【-p 1 が要る理由 —— パッケージを並列に走らせない】
	# go test は既定で **GOMAXPROCS 個のテストバイナリを同時に**走らせる。
	# postgres 側の live 検査は同じ DB にスレッドを**コミットして**作り、
	# Cleanup で消す。thread/usecase の N+1 比較は 2 つの実装を
	# **別々の瞬間に**投げて件数と ID 列の完全一致を求めるので、
	# その間に挿入や削除が挟まると落ちる。
	# 既定の並びは id の降順なので、**挿入も削除も必ず 1 ページ目の先頭に効く。**
	cd $(GO_API_DIR) && \
		DB_TEST_REQUIRE=1 DATABASE_TEST_URL="$(LIVE_DATABASE_URL)" \
		S3_TEST_REQUIRE=1 S3_TEST_ENDPOINT="$(LIVE_S3_ENDPOINT)" \
		S3_TEST_BUCKET=bbs-images \
		S3_TEST_PUBLIC_BASE_URL="$(LIVE_S3_ENDPOINT)/bbs-images" \
		go test -count=1 -p 1 -run '_Live$$|TestS3Storage|TestNew_' \
			./internal/infrastructure/postgres/ \
			./internal/infrastructure/objectstorage/ \
			./internal/thread/usecase/

# カバレッジの下限。下回ると cover が落ちる。
#
# 上げるときは「テストを足した結果として上がった」ときだけにすること。
# 数字を満たすためにテストを書き始めると、
# 「通るが何も守らないテスト」が量産される。
# 目安は 90%。到達したら COVER_MIN を上げてラチェットにする。
COVER_MIN := 83

.PHONY: cover
cover: ## 手書きロジックのカバレッジを測り、下限を下回ったら落とす
	# 【計測対象を絞る理由】
	#   oapigen / sqlcgen : 生成コード。テストを書く対象ではない
	#   infrastructure/   : 実 DB と実 IdP が要る。フェイクで測っても意味が無い
	#                       (SQL が意図どおり動くかは make smoke が見る)
	#   cmd/              : 起動と結線。ここは smoke が実質のカバーになる
	#
	# 全体で率を出すと、生成コードのために意味のないテストを書く圧力が生まれる。
	#
	# 【-coverpkg を使う理由】
	# 既定の -cover は「そのパッケージ自身のテスト」しか数えない。
	# httpapi のテストが usecase を通しても usecase には計上されないため、
	# 実態より低く出る。
	#
	# 【-count=1 が要る理由】
	# **テストキャッシュに当たると、出てくる数字が壊れる。**
	# 「(cached)」で再生されたプロファイルは行数が減り (実測 3115 -> 3055 行)、
	# 同じコードのまま 83.8% が 54.8〜57.1% に化ける。
	# 一度でも同じコマンドを流したあとに測ると下限割れで落ちるので、毎回走らせる。
	# カバレッジは「実行した事実」の記録であって、
	# キャッシュから復元してよい値ではない。
	#
	# 【出力を捨てない】
	# **以前は > /dev/null にしていて、落ちたときに理由が残らなかった。**
	# 「カバレッジ行が出ないまま落ちるが、再実行すると通る」が 2 度起きており、
	# どちらも原因が分からないまま閉じている。成功時は静かなままにしたいので、
	# ログに落として**失敗したときだけ末尾を出す**形にする。
	# 全文は /tmp/cover-test.log に残る。
	# **test と同じく実 DB / S3 の環境変数を空にする。**
	# ここを忘れると、check の `test` だけ直しても `cover` の go test が
	# 環境をそのまま継承し、**実 DB 検査がこれまでどおり走る** ——
	# thread/usecase は計測対象に入っている (レビュー指摘)。
	@cd $(GO_API_DIR) && DATABASE_TEST_URL= S3_TEST_ENDPOINT= DB_TEST_REQUIRE= S3_TEST_REQUIRE= 		pkgs=$$(go list ./internal/... | grep -vE 'oapigen|sqlcgen|/infrastructure/' | paste -sd, -) && 		{ go test -count=1 -coverpkg="$$pkgs" -coverprofile=/tmp/cover.out $$(echo "$$pkgs" | tr ',' ' ') > /tmp/cover-test.log 2>&1 || { echo "カバレッジ計測のテストが失敗しました (全文: /tmp/cover-test.log)"; tail -30 /tmp/cover-test.log; exit 1; }; } && 		total=$$(go tool cover -func=/tmp/cover.out | tail -1 | grep -oE '[0-9]+\.[0-9]+') && 		echo "手書きロジックのカバレッジ: $$total% (下限 $(COVER_MIN)%)" && 		awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { if (t+0 < m+0) { print "下限を下回りました"; exit 1 } }'

.PHONY: cover-html
cover-html: cover ## カバレッジをブラウザで開く (どこが通っていないかを見る)
	cd $(GO_API_DIR) && go tool cover -html=/tmp/cover.out

.PHONY: smoke
smoke: ## 実 DB に対して API を起動し、HTTP 越しに疎通を検証する (既存データは消える)
	# ユニットテストはフェイクのリポジトリで動くため、
	# 「SQL が意図どおり動くか」は検証できない。その穴を埋める。
	@$(MAKE) --no-print-directory up
	# up は「イメージが変わらなければコンテナを作り直さない」。
	# dev の go-api はソースを volume マウントしていてイメージが変わらず、
	# go run は起動時にしかコンパイルしないため、既に動いていると
	# 古いバイナリのまま残る。生成物を作り直した直後や
	# ブランチを切り替えた直後に、古いコードを検証してしまう。
	docker compose restart go-api
	@$(MAKE) --no-print-directory seed
	@echo "API の起動を待っています..."
	@for i in $$(seq 1 30); do \
		curl -sf -o /dev/null http://localhost:8080/healthz && break || sleep 2; \
	done
	BASE_URL=http://localhost:8080 \
	SQL_EXEC="docker compose exec -T postgres psql -U app -d bbs -X -q -c" \
	python3 .github/scripts/smoke-test.py

.PHONY: bench
bench: ## ベンチマークを実行する
	cd $(GO_API_DIR) && go test -bench=. -benchmem -run='^$$' ./...

.PHONY: lint
lint: arch ## Go / TypeScript の静的解析を実行する
	cd $(GO_API_DIR) && golangci-lint run ./...
	cd $(NEXT_DIR) && $(NODE) npx tsc --noEmit
	cd $(NEXT_DIR) && $(NODE) npm run lint

.PHONY: e2e
e2e: ## 管理画面の状態遷移を実ブラウザで検査する (DB 不要)
	# **実バックエンドは使わない。** 応答は page.route で差し替える。
	# ここで測るのは API の挙動ではなく、遅延・失敗・順序に対する
	# クライアントの状態遷移になる —— 「A の応答が B より後に届く」は
	# 実 API では作れない (apps/next-app/playwright.config.ts に理由)。
	#
	# API 側を実 HTTP 越しに見るのは make smoke の役目。重ねない。
	#
	# install は導入済みなら数秒で終わる。**入れ忘れで落ちるのを避ける**
	# ほうが、毎回数秒払うより安い。
	cd $(NEXT_DIR) && $(NODE) npx playwright install chromium
	cd $(NEXT_DIR) && $(NODE) npx playwright test

.PHONY: arch
arch: ## モジュール境界とレイヤの依存方向を検査する
	# 本体用とテスト用の 2 枚を両方通す必要がある。
	# 本体コードには両方が適用され、テストファイルには緩和版だけが効く。
	cd $(GO_API_DIR) && go-arch-lint check
	cd $(GO_API_DIR) && go-arch-lint check --arch-file .go-arch-lint.tests.yml

.PHONY: arch-probe
arch-probe: ## 境界検査そのものが機能しているかをプローブで実測する
	# 「落ちるはずのものが通る」状態を検出する。
	# 設定を読んだだけでは正しさを判断できないため CI で回す
	.github/scripts/arch-probe.sh

# 変異リスト。PR ごとの使い捨てなので既定値は置かない。
MUTATIONS ?=

.PHONY: mutation-probe
mutation-probe: ## テストが実際に効くかを、実装をわざと壊して実測する (MUTATIONS=<変異リスト>)
	# 「テストが通った」は「テストが何かを守っている」ことを意味しない。
	# 実装を壊してもテストが通るなら、そのテストは何も守っていない。
	#
	# arch-probe と違って CI には載せない。壊し方は変更のたびに陳腐化するため、
	# 変異リストは PR ごとの使い捨てにする (書式はスクリプト冒頭)。
	@test -n "$(MUTATIONS)" || { \
		echo "MUTATIONS=<変異リスト> を指定してください。"; \
		echo "書式は .github/scripts/mutation-probe.py の冒頭にあります。"; \
		exit 2; \
	}
	.github/scripts/mutation-probe.py $(MUTATIONS)

PROBE_WORKERS ?= 32

.PHONY: concurrency-probe
concurrency-probe: ## 4 つの並行制御モードを実 DB で比較計測する (PROBE_WORKERS=<並列数>)
	# 同一バイナリのまま COMMENT_POST_MODE だけを差し替え、
	# 同じスレッドへ同時投稿したときに何が起きるかを測る。
	# 設計は docs/adr/0019-comment-concurrency.md。
	#
	# **モードごとにコンテナを作り直すため、数十秒かかる。**
	# CI には載せない (結果は環境の性能に左右されるので、通す/落とすの
	# 基準にできない)。正しさの検査は smoke 側にある。
	@$(MAKE) --no-print-directory up
	@echo "API の起動を待っています..."
	@for i in $$(seq 1 30); do \
		curl -sf -o /dev/null http://localhost:8080/healthz && break || sleep 2; \
	done
	BASE_URL=http://localhost:8080 \
	SQL_EXEC="docker compose exec -T postgres psql -U app -d bbs -X -q" \
	PROBE_WORKERS=$(PROBE_WORKERS) \
	python3 .github/scripts/concurrency-probe.py

.PHONY: verify-log-events
verify-log-events: ## ログの msg がイベント名になっているかを検査する
	# **実行時の検査では届かない範囲を埋める。** make logs-verify は
	# 実際に出たログしか見られないので、エラー経路のログは検査されない。
	# こちらはソースを見るので、呼ばれていないログも全部対象になる。
	#
	# DB も MinIO も要らないため、ログ基盤の検証でここだけが CI に載る
	# (ADR 0010「実装して分かったこと 3」)。
	.github/scripts/verify-log-events.py

.PHONY: verify-constraint-checks
verify-constraint-checks: ## DB の CHECK 制約に、守る検査が付いているかを検査する
	# **Go のテストは、自分が存在しないことを検出できない。**
	# 値の突き合わせは各ドメインのテストが持つが、その形では
	# 「そもそも検査を書き忘れた制約」に誰も気づけない
	# (実際に image で漏れた。ADR 0003 #8)。
	#
	# ここは値を見ない。制約と検査の対応だけを見る。DB は要らない。
	.github/scripts/verify-constraint-checks.py

.PHONY: checker-probe
checker-probe: ## 静的検査そのものが「落とすべきものを落とす」かを実測する
	# **検査は、壊れても出力が変わらない。** 何も検出できない状態でも
	# 「OK」と出て CI は緑になる。実際に verify-log-events.py には
	# 「壊しても終了コード 0」の穴が 2 つ空いていた (2026-08-24)。
	#
	# arch-probe が arch-lint に、mutation-probe が Go のテストに
	# しているのと同じことを、ソースを読む静的検査に対して行う。
	.github/scripts/checker-probe.py

PROBE_VIEWERS ?= 16

.PHONY: viewcount-probe
viewcount-probe: ## 閲覧数の反映方式を実 DB で比較計測する (Phase 4 / ADR 0006)
	# **本命は「閲覧数の更新あり / なしでコメント投稿の直列化失敗率がどう変わるか」。**
	# 無関係に見える機能追加が、別の機能の並行制御を壊すことを数字で示す。
	#
	# concurrency-probe と同じく **CI には載せない** ——
	# 数字が環境の性能に左右されるので、通す / 落とすの基準にできない。
	#
	# **測定中はコンテナを何度も作り直す。** 条件ごとに
	# VIEW_COUNT_MODE を差し替えるため、数分かかる。
	@$(MAKE) --no-print-directory up
	@echo "API の起動を待っています..."
	@for i in $$(seq 1 30); do \
		curl -sf -o /dev/null http://localhost:8080/healthz && break || sleep 2; \
	done
	BASE_URL=http://localhost:8080 \
	SQL_EXEC="docker compose exec -T postgres psql -U app -d bbs -X -q" \
	PROBE_WORKERS=$(PROBE_WORKERS) \
	PROBE_VIEWERS=$(PROBE_VIEWERS) \
	python3 .github/scripts/viewcount-probe.py

.PHONY: bench-dataset
bench-dataset: ## Phase 4 用のデータセットを投入する (2,000 スレッド / 20 万コメント。既存データは消える)
	# **開発用シードとは別物。** あちらは目視で確認できる 5 スレッドで、
	# こちらは「索引が効くかどうかが結果に出る」規模を作る。
	#
	# **既存のデータは消える** (先頭で TRUNCATE している)。
	# 戻すには make seed を流す。
	docker compose exec -T postgres psql -U app -d bbs -X -q -v ON_ERROR_STOP=1 \
		< $(GO_API_DIR)/db/bench/dataset.sql

.PHONY: query-probe
query-probe: ## 一覧・検索・ページ送りのクエリを実測する (Phase 4)
	# 「Phase 4 で測る」と書いたまま残っていた 3 件を片付ける:
	#   - コメント数の集計 (投稿者の JOIN を足してから測っていない)
	#   - 検索クエリのレスポンス分布 (ADR 0012)
	#   - 深いページでカーソルの OR を COALESCE にすべきか (レビュー指摘)
	#
	# **先に make bench-dataset が要る。** 開発シードの 5 スレッドでは
	# 何を測っても意味が無い。
	SQL_EXEC="docker compose exec -T postgres psql -U app -d bbs -X -q" \
	python3 .github/scripts/query-probe.py

# ベンチマークから見た DB の接続先。ホストから直接繋ぐ
# (probe と違って psql 越しではなく、Go が往復するところまで測るため)。
BENCH_DATABASE_URL ?= postgres://app:password@localhost:5432/bbs?sslmode=disable
BENCH_MAX_CONNS ?= 16

.PHONY: nplus1-probe
nplus1-probe: ## N+1 と単一クエリを実 DB で比較計測する (Phase 4 / ADR 0014)
	# **query-probe では測れないものを測る。** あちらは EXPLAIN ANALYZE なので
	# サーバ側の実行時間しか見えず、N+1 のコストの本体である
	# **往復回数とプール待ち**が数字に出ない。ここは Go から実際に往復させる。
	#
	# **先に make bench-dataset が要る。** 開発シードの 5 スレッドでは
	# 1 ページぶんの往復すら発生しない。
	#
	# 先に「2 つの実装が同じ結果を返すこと」を検査してから測る ——
	# 片方が投稿者を解決していなければ、速いのは当たり前で
	# 意味のある数字にならない (ADR 0014)。
	#
	# **CI には載せない** (concurrency-probe / viewcount-probe と同じ理由)。
	cd $(GO_API_DIR) && DATABASE_TEST_URL="$(BENCH_DATABASE_URL)" \
		go test ./internal/thread/usecase/ -run TestNPlusOneMatchesSingleQuery_Live -v -count=1
	cd $(GO_API_DIR) && DATABASE_TEST_URL="$(BENCH_DATABASE_URL)" \
		BENCH_MAX_CONNS=$(BENCH_MAX_CONNS) \
		go test ./internal/thread/usecase/ -run='^$$' \
			-bench='BenchmarkThreadList' -benchmem -benchtime=3s -count=1

# 段と変種。既定は 20 万 → 200 万 → 2,000 万行、分割なしと HASH 8。
PROBE_SCALES ?= 200000,2000000,20000000
PROBE_PARTITIONS ?= 0,8

.PHONY: partition-probe
partition-probe: ## comments の 8 分割がどの規模から効き始めるかを実測する (Phase 4)
	# **本番の comments は触らない。** bench_part スキーマに
	# 同じ列・同じ索引の表を「分割なし」と「HASH 8 分割」で作り、
	# 同一データを入れて同じクエリを流す。
	#
	# **数十分かかる。** 2,000 万行を 2 変種ぶん投入するため。
	# 途中経過を出しながら進む。空き容量が足りない段は測らずに報告して止まる。
	#
	# **CI には載せない** (他の probe と同じ理由)。
	SQL_EXEC="docker compose exec -T postgres psql -U app -d bbs -X -q" \
	PROBE_SCALES=$(PROBE_SCALES) \
	PROBE_PARTITIONS=$(PROBE_PARTITIONS) \
	python3 -u .github/scripts/partition-probe.py

# 負荷の段。HTTP は並列数、DB は接続数。
PROBE_LEVELS ?= 1,2,4,8,16,32,64,128
PROBE_DB_LEVELS ?= 1,2,4,8,16,32,64
PROBE_DURATION ?= 5s
# 接続プール上限の段。**この段だけコンテナを作り直す**ので時間がかかる。
PROBE_POOL_LEVELS ?= 4,8,12,16,20,32,64
PROBE_POOL_TEST_LEVELS ?= 16,64

.PHONY: scale-probe
scale-probe: ## 読み取りのスケール限界を実測する (Phase 4 / ADR 0009)
	# **負荷生成側の天井を先に測る。** compose の単一マシンでは
	# 生成側が先に飽和しうるので、校正なしの RPS は解釈できない。
	# /healthz (DB を触らない) の天井と /threads を比べて、
	# **どちら側が飽和したか**を数字で出す。
	#
	# 負荷生成は go-api コンテナの中で走らせる ——
	# ホストから叩くと Docker のポート転送が先に律速になる。
	#
	# **先に make bench-dataset が要る。**
	# **CI には載せない** (他の probe と同じ理由)。
	@$(MAKE) --no-print-directory up
	@echo "API の起動を待っています..."
	@for i in $$(seq 1 30); do \
		curl -sf -o /dev/null http://localhost:8080/healthz && break || sleep 2; \
	done
	# **待つだけでは失敗を検出できない。** ループを抜けても起動していない場合、
	# 先へ進んで Python 側の無関係なエラーで落ちるので原因が読めない。
	@curl -sf -o /dev/null http://localhost:8080/healthz || { \
		echo "API が起動しませんでした。docker compose logs go-api を見てください。"; \
		exit 1; \
	}
	PROBE_LEVELS=$(PROBE_LEVELS) \
	PROBE_DB_LEVELS=$(PROBE_DB_LEVELS) \
	PROBE_DURATION=$(PROBE_DURATION) \
	PROBE_POOL_LEVELS=$(PROBE_POOL_LEVELS) \
	PROBE_POOL_TEST_LEVELS=$(PROBE_POOL_TEST_LEVELS) \
	python3 -u .github/scripts/scale-probe.py

.PHONY: partition-probe-clean
partition-probe-clean: ## partition-probe が作った表を消す
	# **数 GB 残る。** 測り終わったら流すこと。
	docker compose exec -T postgres psql -U app -d bbs -X -q \
		-c "DROP SCHEMA IF EXISTS bench_part CASCADE;"

.PHONY: verify-generated
verify-generated: generate ## 生成物がコミット済みの内容と一致するか検査する
	@git diff --exit-code -- \
		$(GO_API_DIR)/internal/httpapi/oapigen \
		$(GO_API_DIR)/internal/infrastructure/postgres/sqlcgen \
		$(NEXT_DIR)/schema.d.ts \
		|| (echo ""; \
		    echo "生成物が最新ではありません。'make generate' の結果をコミットしてください。"; \
		    exit 1)

.PHONY: tools
tools: ## 開発ツール (sqlc / golangci-lint / go-arch-lint) をインストールする
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION)
	go install github.com/fe3dback/go-arch-lint@$(GO_ARCH_LINT_VERSION)

.PHONY: verify-tidy
verify-tidy: ## go.mod / go.sum が最新か検査する
	# **check が「CI と同じ検証」を名乗るなら、これも要る。**
	# CI の Go Checks は最初にこれを回している。ここに無かったせいで、
	# make check が通ったまま CI だけが赤くなる状態を実際に作った
	# (import を足したのに go mod tidy を忘れ、直接依存が
	#  indirect のままになっていた)。
	#
	# 【HEAD と比べてはいけない】
	# 初版は git diff で HEAD と比較しており、**依存を足した未コミットの状態では
	# 必ず落ちた**。CI は毎回クリーンなチェックアウトなので気づけない差になる。
	# 見たいのは「tidy が go.mod を書き換えるか」であって、
	# 「コミット済みと違うか」ではない。実行前の内容と比べる。
	# 一時ファイルは mktemp で取る。固定パスにすると、同じマシンで
	# 2 つの実行 (別チェックアウト / make -j) が互いの控えを踏む。
	@cd $(GO_API_DIR) && \
		tmp=$$(mktemp -d) && \
		trap 'rm -rf "$$tmp"' EXIT && \
		cp go.mod go.sum "$$tmp/" && \
		go mod tidy && \
		if ! diff -q go.mod "$$tmp/go.mod" > /dev/null || \
		   ! diff -q go.sum "$$tmp/go.sum" > /dev/null; then \
			echo "go.mod / go.sum が最新ではありません。'go mod tidy' の結果をコミットしてください。"; \
			diff "$$tmp/go.mod" go.mod || true; \
			exit 1; \
		fi

.PHONY: check
check: lint test cover verify-tidy verify-generated verify-log-events verify-constraint-checks checker-probe arch-probe e2e ## CI と同じ検証をローカルで一通り実行する (DB 不要)

.PHONY: check-all
# **test-live は smoke の後ろに置く。** 前に置くと、スタックが落ちている
# 状態では test-live が接続に失敗し、DB_TEST_REQUIRE=1 なので
# **スキップではなく Fatal** で止まる (レビュー指摘)。
# いまは test-live 自身が up に依存するので順序に関係なく立つが、
# 依存を外したときに黙って壊れないよう、順序でも意図を残しておく。
#
# 【手元の DB は消える】
# smoke が make seed を呼び、db/seed/seed.sql の先頭が
# TRUNCATE TABLE comments, threads RESTART IDENTITY CASCADE。
# **check-all を通すと、手元のスレッドとコメントは毎回消える。**
# ベンチ用データセットを入れてある場合は make bench-dataset で入れ直すこと。
#
# **「開発用シードの状態に戻る」ではない。** seed.sql が入れるのは 5 件だが、
# そのあと smoke-test.py が実 API を叩いて自分の行を残す (実測 9 件 / max(id)=9)。
# **check-all は DB 状態に対して冪等ではない** —— リセット手段として使わないこと。
#
# test-live 単体は消さない (up-seeded ではなく up に依存させてある) が、
# **check-all の中では smoke が先に消す。** 単体で回したときと
# 通しで回したときで、test-live が見るデータ規模が変わる。
check-all: check smoke test-live logs-verify ## check に加えて実 DB / ログ基盤まで確認する (手元の DB は消える)
