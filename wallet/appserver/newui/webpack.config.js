const path = require('path')
const HtmlWebpackPlugin = require('html-webpack-plugin')
const ESLintPlugin = require('eslint-webpack-plugin')
const MiniCssExtractPlugin = require('mini-css-extract-plugin')

module.exports = {
  entry: './src/index.tsx',
  devtool: 'inline-source-map',
  module: {
    rules: [
      {
        test: /\.tsx?$/,
        // onlyCompileBundledFiles: by default ts-loader type-checks every file
        // matched by tsconfig.json's project scope, not just files actually
        // reached from the entry point — which pulled *.test.tsx (never
        // imported by production code) into the production build.
        use: { loader: 'ts-loader', options: { onlyCompileBundledFiles: true } },
        exclude: /node_modules/
      },
      {
        test: /\.s[ac]ss$/i,
        use: [
          MiniCssExtractPlugin.loader,
          // Translates CSS into CommonJS
          {
            loader: 'css-loader',
            options: {
              url: false
            }
          },
          // Compiles Sass to CSS
          {
            loader: 'sass-loader',
            options: {
              api: 'modern',
              implementation: require('sass')
            }
          }
        ]
      },
      {
        test: /\.(woff2?|png|svg|jpe?g|gif)$/i,
        type: 'asset/resource',
        generator: {
          emit: false
        }
      }
    ]
  },
  resolve: {
    extensions: ['.tsx', '.ts', '.js'],
    alias: {
      '@caribou/shared': path.resolve(__dirname, '../shared/src')
    }
  },
  output: {
    filename: 'bundle.js',
    path: path.resolve(__dirname, 'dist'),
    publicPath: '/js/'
  },
  plugins: [
    new MiniCssExtractPlugin({
      filename: 'bundle.css'
    }),
    new HtmlWebpackPlugin({
      template: 'src/index.html',
      inject: false
    }),
    // For some reason, the watch script does not output eslint errors when
    // on its initial run, but does once any file is edited. TODO: fix
    new ESLintPlugin({
      context: __dirname,
      extensions: ['ts', 'tsx'],
      emitError: true,
      emitWarning: true
    })
  ]
}
