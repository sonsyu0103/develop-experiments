.DEFAULT_GOAL := help

# ローカルで動かす kind のクラスタ名とイメージ名。
NAME    := go-service
IMAGE   := $(NAME):local
CLUSTER ?= kind
# 版を固定した kind / kubectl を使うなら差し替える (make kind-deploy KUBECTL=...)。
KIND    ?= kind
KUBECTL ?= kubectl --context kind-$(CLUSTER)

# 版の固定。@latest にしない —— ある日ルールが増えて CI が突然赤くなるのを避ける。
# oapi-codegen は go.mod の tool ディレクティブが固定している。
GOLANGCI_LINT_VERSION := v2.12.2

.PHONY: help
help: ## このヘルプを表示する
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: generate
generate: ## api/openapi.yaml から Go のスタブを生成する
	go tool oapi-codegen -config oapi-codegen.yaml api/openapi.yaml

.PHONY: verify-generated
verify-generated: ## 生成物が仕様書と一致するか検査する (作業ツリーは書き換えない)
	# **git に頼らない。** git diff で見ると、git init 前のディレクトリでは
	# 「リポジトリではない」で落ち、それを「ずれている」と誤って報告する。
	# 一時ディレクトリに作り直して、手元の生成物と比べる。
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	sed "s#^output: .*#output: $$tmp/oapigen.gen.go#" oapi-codegen.yaml > "$$tmp/config.yaml"; \
	go tool oapi-codegen -config "$$tmp/config.yaml" api/openapi.yaml; \
	diff -u internal/httpapi/oapigen/oapigen.gen.go "$$tmp/oapigen.gen.go" \
		|| { echo "NG: 生成物が仕様書とずれている。make generate する"; exit 1; }; \
	echo "OK: 生成物は仕様書と一致している"

.PHONY: test
test: ## テスト (race detector + カバレッジ)
	go test -race -cover ./...

.PHONY: lint
lint: ## 静的解析
	go vet ./...
	golangci-lint run ./...

.PHONY: run
run: ## 手元で起動する (http://localhost:8080)
	go run ./cmd/api

.PHONY: image
image: ## 本番用のイメージを作る
	docker build --target prod -t $(IMAGE) .

.PHONY: k8s-render
k8s-render: ## k8s のマニフェストを組み立てる (クラスタは要らない)
	$(KUBECTL) kustomize deploy/k8s

.PHONY: kind-deploy
kind-deploy: image ## kind のクラスタにイメージを入れて載せる
	$(KIND) load docker-image $(IMAGE) --name $(CLUSTER)
	$(KUBECTL) apply -k deploy/k8s
	$(KUBECTL) -n $(NAME) rollout status deployment/$(NAME) --timeout=180s
