FROM node:20-alpine

WORKDIR /app

# 依存関係をキャッシュするために先にコピー
COPY package.json package-lock.json ./
RUN npm ci

COPY . .

# 開発用サーバーを立ち上げる
CMD ["npm", "run", "dev"]