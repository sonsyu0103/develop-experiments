# Next.js 16 は Node.js 20.9 以降を要求する。LTS の 22 を使う。
#
# **ステージを分ける** (go-api/Dockerfile と同じ形)。
# 手元の compose は dev を、AWS では prod を使う。
# 1 つのファイルに両方あることで、「本番だけ別物」になるのを防ぐ。

FROM node:22-alpine AS base

WORKDIR /app

# 依存関係をキャッシュするために先にコピー
COPY package.json package-lock.json ./
RUN npm ci

# ---------------------------------------------------------------------------
# 開発用 (compose が target: dev で使う)
# ---------------------------------------------------------------------------
FROM base AS dev

COPY . .

EXPOSE 3000

# 開発用サーバーを立ち上げる
CMD ["npm", "run", "dev"]

# ---------------------------------------------------------------------------
# 本番ビルド
# ---------------------------------------------------------------------------
FROM base AS builder

# **NEXT_PUBLIC_* はビルド時に定数へ置換される。**
# 実行時の環境変数では上書きできないので、ここで渡す
# (app/lib/api.ts が「本番のイメージを作る段になったら、
# ビルド引数として渡すこと」と書いているのはこのこと)。
#
# **既定を相対パスにする。** エッジ (Caddy / CloudFront) の背後では
# 画面と API が同一オリジンに載るので、ホスト名を焼き込む必要がない。
# CloudFront のドメインは apply するまで決まらないので、
# ホスト名を焼く設計だと「イメージを作るには URL が要る、
# URL を得るには apply が要る」という循環に入る。
ARG NEXT_PUBLIC_API_URL=/api
ENV NEXT_PUBLIC_API_URL=${NEXT_PUBLIC_API_URL}

COPY . .

# next.config.ts の output: 'standalone' により
# .next/standalone に実行に要るものだけが出る。
RUN npm run build

# ---------------------------------------------------------------------------
# 本番実行
# ---------------------------------------------------------------------------
FROM node:22-alpine AS prod

WORKDIR /app

ENV NODE_ENV=production
ENV PORT=3000

# **0.0.0.0 で待ち受ける。**
# standalone の既定は localhost なので、これが無いと
# **コンテナの外から届かない** —— ALB のヘルスチェックが延々と失敗し、
# タスクが起動と終了を繰り返す。
ENV HOSTNAME=0.0.0.0

# **root で動かさない。**
RUN addgroup -g 1001 -S nodejs && adduser -S nextjs -u 1001

# standalone は server.js と最小限の node_modules を含む。
COPY --from=builder --chown=nextjs:nodejs /app/.next/standalone ./
# 静的アセットは standalone に含まれないので別途コピーする。
# **これを忘れると CSS と JS が 404 になり、画面が崩れる。**
COPY --from=builder --chown=nextjs:nodejs /app/.next/static ./.next/static

USER nextjs

EXPOSE 3000

CMD ["node", "server.js"]
