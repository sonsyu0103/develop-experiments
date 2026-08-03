# Next.js 16 は Node.js 20.9 以降を要求する。
# LTS の 22 を使う。
FROM node:22-alpine

WORKDIR /app

# 依存関係をキャッシュするために先にコピー
COPY package.json package-lock.json ./
RUN npm ci

COPY . .

EXPOSE 3000

# 開発用サーバーを立ち上げる
CMD ["npm", "run", "dev"]
