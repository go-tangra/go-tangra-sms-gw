import js from '@eslint/js'
import ts from 'typescript-eslint'
import vue from 'eslint-plugin-vue'
import vueParser from 'vue-eslint-parser'
import globals from 'globals'
import { freyaRules } from '@go-tangra/ui/eslint'

export default [
  { ignores: ['dist/**', 'node_modules/**', 'src/api/schema.d.ts', 'playwright-report/**', 'test-results/**'] },
  js.configs.recommended,
  ...ts.configs.recommended,
  ...vue.configs['flat/recommended'],
  { languageOptions: { globals: { ...globals.browser } } },
  { files: ['**/*.vue'], languageOptions: { parser: vueParser, parserOptions: { parser: ts.parser } } },
  { rules: freyaRules },
]
