FROM golang:1.26-alpine

WORKDIR /app

# 依存関係をキャッシュするために先にコピー
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# ホットリロード（コード変更の自動反映）のために air を入れてもいいけれど、
# まずは一番シンプルに go run で立ち上げる設定にするね
CMD ["go", "run", "main.go"]