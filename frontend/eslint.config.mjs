import js from '@eslint/js';
import ts from 'typescript-eslint';
import hooks from 'eslint-plugin-react-hooks';
import globals from 'globals';

export default ts.config(
  {
    ignores: [
      'dist/**',
      '.test-dist/**',
      'node_modules/**',
      'public/**',
      'playwright-report/**',
      'test-results/**',
    ],
  },
  js.configs.recommended,
  ...ts.configs.recommended,
  { languageOptions: { globals: { ...globals.browser, ...globals.node } } },
  {
    files: ['src/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': hooks },
    rules: {
      ...hooks.configs.recommended.rules,
      'no-restricted-imports': [
        'error',
        {
          patterns: [
            {
              group: ['**/app/**'],
              message: 'App composition belongs above feature and shared code.',
            },
          ],
        },
      ],
    },
  },
  {
    files: ['src/app/**/*', 'src/main.tsx'],
    rules: { 'no-restricted-imports': 'off' },
  },
);
