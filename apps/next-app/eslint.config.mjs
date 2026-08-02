// ESLint フラット設定 (ESLint 9 以降の形式)。
//
// Next.js 16 で `next lint` は削除されたため、eslint を直接呼び出す。
// package.json の lint スクリプトを参照。
//
// ESLint は 9 系に固定している。eslint-config-next の peerDependencies は
// >=9.0.0 だが、同梱の eslint-plugin-react が ESLint 10 で削除された
// context.getFilename() を呼ぶため、10 系では起動時に落ちる。
import nextCoreWebVitals from 'eslint-config-next/core-web-vitals';
import nextTypeScript from 'eslint-config-next/typescript';

const config = [
  {
    // 生成物と成果物は検査しない
    ignores: [
      '.next/**',
      'node_modules/**',
      'out/**',
      'next-env.d.ts',
      // openapi.yaml から自動生成されるため手を入れない
      'schema.d.ts',
    ],
  },

  // Core Web Vitals に関わるルールを含む Next.js 推奨設定
  ...nextCoreWebVitals,
  // TypeScript 向けルール
  ...nextTypeScript,

  {
    rules: {
      // API から受け取った値を any で素通しさせない。
      // schema.d.ts で型が生成されている以上、any を使う理由がない。
      '@typescript-eslint/no-explicit-any': 'error',
      // 未使用変数はエラー。ただし _ 始まりは意図的な無視として許可する。
      '@typescript-eslint/no-unused-vars': [
        'error',
        { argsIgnorePattern: '^_', varsIgnorePattern: '^_' },
      ],
    },
  },
];

export default config;
