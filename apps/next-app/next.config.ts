import type { NextConfig } from 'next';

const nextConfig: NextConfig = {
  // **コンテナに載せるために standalone で出す。**
  //
  // 既定のビルド成果物は node_modules 一式を前提にするので、
  // イメージに開発依存まで含めることになる。standalone は
  // 実行に要るものだけを .next/standalone にまとめ、
  // `node server.js` で起動できる形にする。
  //
  // dev には影響しない (build のときだけ効く)。
  output: 'standalone',
};

export default nextConfig;
