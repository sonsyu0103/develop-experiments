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

.PHONY: up-seeded
up-seeded: up ## 起動したうえでシードデータまで投入する (既存データは消える)
	# up と分けているのは、seed.sql が TRUNCATE から始まるため。
	# make up にまとめると、起動のたびに手入力のデータが消える。
	@$(MAKE) --no-print-directory seed

.PHONY: down
down: ## 全サービスを停止する (データは残る)
	docker compose down

.PHONY: clean
clean: ## 全サービスを停止し、DB のデータも消す
	docker compose down -v

.PHONY: logs
logs: ## ログを追尾する
	docker compose logs -f

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
test: ## Go のテストを実行する (race detector + カバレッジ)
	cd $(GO_API_DIR) && go test -race -cover ./...

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
	@cd $(GO_API_DIR) && 		pkgs=$$(go list ./internal/... | grep -vE 'oapigen|sqlcgen|/infrastructure/' | paste -sd, -) && 		go test -count=1 -coverpkg="$$pkgs" -coverprofile=/tmp/cover.out $$(echo "$$pkgs" | tr ',' ' ') > /dev/null && 		total=$$(go tool cover -func=/tmp/cover.out | tail -1 | grep -oE '[0-9]+\.[0-9]+') && 		echo "手書きロジックのカバレッジ: $$total% (下限 $(COVER_MIN)%)" && 		awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { if (t+0 < m+0) { print "下限を下回りました"; exit 1 } }'

.PHONY: cover-html
cover-html: cover ## カバレッジをブラウザで開く (どこが通っていないかを見る)
	cd $(GO_API_DIR) && go tool cover -html=/tmp/cover.out

.PHONY: smoke
smoke: ## 実 DB に対して API を起動し、HTTP 越しに疎通を検証する
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
check: lint test cover verify-tidy verify-generated arch-probe e2e ## CI と同じ検証をローカルで一通り実行する (DB 不要)

.PHONY: check-all
check-all: check smoke ## check に加えて実 DB での疎通確認まで行う
