# Privacy Policy

Caribou has no telemetry, no analytics, no accounts, and no Bisoncraft server that your wallet reports to. We do not collect your seed, your password, your addresses, your balances, or your transactions.

## What stays on your device

Your encrypted keys, settings, transaction history, and the record that you accepted these terms are stored only on your device. Uninstalling the app or deleting its data directory removes them. Back up your seed first.

## What leaves your device

Caribou has to contact third parties to work. When it does, those parties see your device's IP address and the request itself.

- **Blockchain servers.** To read balances, fetch history, estimate fees, and broadcast transactions, Caribou connects to public blockchain servers and RPC endpoints for each asset you use. They can see the addresses and transactions you query.
- **Price data.** Caribou fetches exchange rates from public price services, including dcrdata, CoinPaprika, and Messari.
- **Swap provider.** When you request a swap quote or start a swap, Caribou sends the swap details to the NEAR Intents service, including the assets, the amount, and the deposit and refund addresses. That service has its own privacy practices, which we do not control.
- **Block explorers.** If you open a transaction in a block explorer, your browser contacts that site.

We do not receive any of these requests, and we do not operate these services.

## Hiding your IP address

You can start Caribou with the `--torproxy` option to route outbound connections through a Tor proxy you provide. Without it, third parties see your real IP address.

## Public blockchains

Blockchain transactions are public and permanent. Anyone can see an address's balance and history. Caribou cannot remove or hide what is recorded on a public blockchain.
