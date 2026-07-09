# Local Automation with API Tokens

This guide covers driving a running `caribou` instance from scripts, a CLI, or an
MCP server using the appserver's HTTP API and a local API token.

There is no separate RPC server. The appserver that serves the web UI is the
single machine-facing interface; automation clients are just additional callers
of the same API. The only thing that differs from the browser is how a
non-browser client authenticates.

## The model

The browser authenticates with a session cookie minted by a password login. A
non-browser client instead holds a **local API token** and sends it in an
`X-API-Token` header. The token:

- **is localhost-only.** It is accepted only when the request originates from a
  loopback peer (`127.0.0.1` / `::1`) on the same machine, regardless of how the
  server is bound. A token leaked off-box is useless.
- **survives logout.** Logging out of the web UI deauthenticates browser
  sessions but does not invalidate the API token, so automation keeps working.
- **does not unlock spending.** The token authenticates the request, but
  `send` still requires the app password in the request body. Reads need only
  the token.

Only the SHA-256 hash of the token is persisted; the raw value is shown once at
creation and cannot be recovered. Generating a new token replaces any previous
one.

## Enabling and bootstrapping

The API is already served on loopback; nothing needs to be turned on. You only
need to mint a token, which is a one-time password-authenticated step.

The server address depends on the network (see the `Web server listening on ...`
line at startup). The examples below use `127.0.0.1:5758`; on `--simnet` the
default is `127.0.0.3:5758`.

```sh
B=http://127.0.0.1:5758

# 1. Log in once with the app password, saving the session cookies.
curl -s -c jar.txt $B/api/login \
  -H 'content-type: application/json' \
  -d '{"pass":"YOUR_APP_PASSWORD"}'

# 2. Mint a local API token (must be run from the same machine).
curl -s -b jar.txt $B/api/generateapitoken \
  -H 'content-type: application/json' -d '{}'
#  -> {"ok":true,"token":"<hex>"}   (shown once — store it securely)
```

Store the returned token wherever your automation keeps secrets. From here on,
no password login and no cookie jar are needed.

## Calling the API

Send the token in the `X-API-Token` header. Every request uses
`content-type: application/json` and a JSON body.

```sh
B=http://127.0.0.1:5758
TOKEN=<hex>

# Balance (read — token only)
curl -s $B/api/balance \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"assetID":0}'

# New deposit address
curl -s $B/api/depositaddress \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"assetID":0}'

# Validate an address
curl -s $B/api/validateaddress \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"assetID":0,"addr":"bc1q..."}'

# Estimate a send fee
curl -s $B/api/txfee \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"assetID":0,"address":"bc1q...","value":100000000,"subtract":false}'

# Transaction history
curl -s $B/api/txhistory \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"assetID":0,"n":10}'

# Send (the app password goes in "pw", not a header)
curl -s $B/api/send \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' \
  -d '{"pw":"YOUR_APP_PASSWORD","assetID":0,"value":150000000,"address":"bc1q...","subtract":false}'
```

`assetID` is the BIP-44 coin type (0 = BTC, 60 = ETH, ...). Amounts are in the
asset's smallest unit (satoshis for BTC).

### Common endpoints

| Route                   | Reads/Writes | App password?       |
| ----------------------- | ------------ | ------------------- |
| `/api/user`             | read         | no                  |
| `/api/balance`          | read         | no                  |
| `/api/depositaddress`   | write        | no                  |
| `/api/validateaddress`  | read         | no                  |
| `/api/txfee`            | read         | no                  |
| `/api/txhistory`        | read         | no                  |
| `/api/send`             | write        | **yes** (`"pw"`)    |
| `/api/generateapitoken` | write        | session (bootstrap) |
| `/api/revokeapitoken`   | write        | token or session    |

These are a subset; any endpoint behind the authenticated API accepts the token.

## Errors

- **Authentication failures return `401`** with the body
  `not authorized - login first` (missing token, wrong token, or a token
  presented from a non-loopback peer).
- **Business errors return `200`** with `{"ok":false,"msg":"...","code":N}`, for
  example a wallet that is not configured, an invalid address, or insufficient
  funds.
- **`400`** for malformed JSON, **`415`** for a non-JSON content type, **`404`**
  for an unknown route.

## Revoking

Revoke the current token at any time. This is immediate; the next request using
it gets `401`. Other sessions (such as the browser) are unaffected.

```sh
curl -s $B/api/revokeapitoken \
  -H "X-API-Token: $TOKEN" -H 'content-type: application/json' -d '{}'
```

To rotate, just generate a new token; the old hash is overwritten.

## Notes and caveats

- **Same-machine only.** The token is rejected from any non-loopback peer.
  Combined with the default loopback bind, the automation surface stays on the
  box.
- **Spending is still password-gated.** `send` (and other password-bearing
  actions) require the app password in the body even with a valid token.
- **One token at a time.** Generating a token replaces the previous one. There
  is a single local-token slot.
- **Shared session semantics.** The wallet has one logged-in core shared by all
  clients. A `POST /api/logout` from any client locks wallets and ends browser
  sessions for everyone (the API token itself keeps authenticating). Automation
  should generally avoid calling `/api/logout`.
