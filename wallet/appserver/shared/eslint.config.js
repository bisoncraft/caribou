// The shared core is mostly framework-agnostic, but it also holds a few shared
// presentational components (.tsx), so JSX and the React plugin are enabled.
const { FlatCompat } = require('@eslint/eslintrc')
const globals = require('globals')

const compat = new FlatCompat({ baseDirectory: __dirname })

module.exports = [
  ...compat.extends('standard', 'plugin:@typescript-eslint/recommended', 'plugin:react/recommended'),
  {
    files: ['**/*.ts', '**/*.tsx'],
    languageOptions: {
      parser: require('@typescript-eslint/parser'),
      parserOptions: {
        warnOnUnsupportedTypeScriptVersion: false,
        ecmaFeatures: {
          jsx: true
        }
      },
      globals: {
        ...globals.browser,
        ...globals.node,
        ...globals.es2021
      }
    },
    settings: {
      react: {
        version: 'detect'
      }
    },
    rules: {
      '@typescript-eslint/no-use-before-define': 'off',
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/indent': 'off',
      'no-use-before-define': 'off',
      'no-trailing-spaces': 'error',
      'no-console': ['off'],
      'no-alert': 'error',
      'no-eval': 'error',
      'no-implied-eval': 'error',
      'object-shorthand': 'off',
      'quote-props': ['error', 'consistent'],
      'object-property-newline': 'off',
      'react/react-in-jsx-scope': 'off',
      'react/prop-types': 'off'
    }
  }
]
