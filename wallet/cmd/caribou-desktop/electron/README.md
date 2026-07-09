# Building Caribou with Electron

This guide provides instructions on how to build the Caribou application using Electron. Follow the steps below to set up your development environment and compile the application.

## Prerequisites

Before you begin, ensure you have the following installed on your system:

- [Node.js](https://nodejs.org/) (version 14 or higher)
- [npm](https://www.npmjs.com/get-npm) (comes with Node.js)
- [Golang](https://golang.org/dl/) (version 1.23.0 or higher)

## Steps to Build Caribou

1. Clone the Repository and ensure you're in the `caribou-desktop/electron` directory:

   ```bash
   cd caribou-desktop/electron
   ```

2. Run `npm ci` to set up the environment. The `./prepare.sh` script is auto executed by `npm ci` after `npm ci` is done.
   To include Monero (XMR) support, run `prepare.sh` manually with the `--xmr` flag:

   ```bash
   ./prepare.sh --xmr
   ```

   If you just want to run the application without building it, you can use the following command after `npm ci`:

   ```bash
   npm start
   ```

3. Run the build script:

   ```bash
   ./pkg.sh
   ```

   After the build process completes, you can find the compiled application in the `caribou-desktop/installers` directory.
