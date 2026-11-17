# syntax=docker/dockerfile:1

# **patch まで固定する。** 浮動タグ (golang:1.26-alpine) だと、ビルドマシンに
# 残っている古い 1.26.x で本番のバイナリが作られうる。公式イメージは
# GOTOOLCHAIN=local なので go.mod の toolchain 行も効かない。
# go.mod の toolchain と CI の GO_VERSION と揃える (make verify-go-version)。
FROM golang:1.26.11-alpine AS base
WORKDIR /app
# go.mod / go.sum だけ先にコピーし、依存のダウンロードをキャッシュに残す。
COPY go.mod go.sum ./
RUN go mod download

# 開発用。ソースはボリュームで差し込む。
FROM base AS dev
EXPOSE 8080
CMD ["go", "run", "./cmd/api"]

FROM base AS builder
COPY . .
# 静的リンク (distroless/static で動かすため)。-trimpath はビルドマシンの
# 絶対パスを消す。-s -w はデバッグ情報を落としてサイズを縮める。
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# 本番用。シェルもパッケージマネージャも無い。
FROM gcr.io/distroless/static-debian12:nonroot AS prod
COPY --from=builder /out/api /api
# **UID を数字で書く。** 名前 (nonroot) だと kubelet が root でないと確かめられず、
# Pod の runAsNonRoot だけで止まる (掲示板の ADR 0026 で実際に踏んだ)。
# 65532 は distroless の nonroot。
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/api"]
