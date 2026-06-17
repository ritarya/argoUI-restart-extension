const path = require('path');

module.exports = {
  entry: './src/index.tsx',
  output: {
    filename: 'extension.js',
    path: path.resolve(__dirname, 'dist'),
    // IIFE so ArgoCD can execute it as a self-contained script
    library: { type: 'window' },
  },
  resolve: {
    extensions: ['.tsx', '.ts', '.js'],
  },
  module: {
    rules: [
      {
        test: /\.tsx?$/,
        use: 'ts-loader',
        exclude: /node_modules/,
      },
    ],
  },
  externals: {
    // React and ReactDOM are provided by the ArgoCD host at runtime
    react: 'React',
    'react-dom': 'ReactDOM',
  },
};
