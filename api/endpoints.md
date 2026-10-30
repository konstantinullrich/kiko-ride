# Kiko API endpoints

All endpoints are under `/api/v1`, take and return JSON, and report errors as

```json
{ "error": "human readable message" }
```

Each driver is paid on the chain their public key belongs to: a Cardano
`acct_shared_xvk1…` key means fares in **lovelace** (1 ADA = 1,000,000
lovelace), a BSV `xpub…`/`tpub…` key fares in **satoshis**. BSV is available
only when the server enables it. Times are RFC 3339.
Coordinates are `{ "lat": 52.52, "lng": 13.37 }`. State is kept in SQLite.

## Ride lifecycle

```
requested ──(driver accepts)──> accepted ──(rider pays via x402)──> paid ──(driver payout)──> paid_out
```

1. The rider creates a ride (`POST /ride`). It is `requested`.
2. A driver finds it (`GET /driver/drive/available`) and accepts it
   (`POST /driver/drive/{ride_id}`). It is `accepted`, and the fare and
   `pay_to` address are fixed.
3. The rider polls `GET /ride/{id}`. While `accepted`, that call answers
   `402 Payment Required` until the rider pays the fare with x402. The paid
   response reveals the car. It is `paid`.
4. The driver builds and signs a transaction that spends the ride address and
   posts it to `POST /driver/drive/{ride_id}/payout`. The server adds its
   signature and submits it. It is `paid_out`.

### Ride address (2-of-3 multisig)

Each ride is paid to its own 2-of-3 multisig output that any two of three keys
can spend:

| Key    | Source                                                             |
| ------ | ------------------------------------------------------------------ |
| Server | Kiko's server key, account `0'` of the server mnemonic             |
| Driver | The driver's `public_key`, any account                             |
| Rider  | The rider's `cosigner_key`, a public key for this ride used as is; or, if none is given, the server's fallback key, account `1'` |

The server and driver keys are account keys on the driver's chain, derived
for each ride at its `address_index` (role 0). The rider's key is not
derived: the rider checks it appears in `pay_to` before paying, and needs no
index to sign with it later.

| Chain   | Server and driver account keys                  | Their ride key                           | Rider key                     | Locked by |
| ------- | ----------------------------------------------- | ---------------------------------------- | ----------------------------- | --------- |
| Cardano | CIP-1854 `acct_shared_xvk1…`                    | `m/1854'/1815'/account'/0/address_index` | `addr_shared_vk1…` (or `addr_shared_xvk1…`) | native script `atLeast 2`, at its script address (`addr1w…`/`addr_test1w…`) |
| BSV     | BIP-32 `xpub…`/`tpub…` at `m/48'/236'/account'` | `m/48'/236'/account'/0/address_index`    | compressed public key, 33 bytes hex (`02…`/`03…`) | bare `OP_2 <keys> OP_3 OP_CHECKMULTISIG` output, keys sorted (BIP-67); it has no address |

If the rider gives no key the ride is `custodial` and the server holds two of
the three keys.

## Ride object

Returned by every ride endpoint. Fields appear as the ride progresses.

```json
{
  "id": "9f2c…",
  "status": "paid",
  "start_address": "Berlin Hbf",
  "end_address": "Alexanderplatz",
  "start": { "lat": 52.5251, "lng": 13.3694 },
  "end": { "lat": 52.5219, "lng": 13.4132 },
  "distance_km": 3.01,
  "pickup_time": "2026-11-01T09:00:00Z",
  "custodial": false,

  "network": "cardano:preprod",
  "fare": 3505000,
  "fare_lovelace": 3505000,
  "pay_to": "addr_test1w…",
  "eta_minutes": 4,
  "eta_km": 1.2,
  "car": {
    "brand": "VW",
    "model": "ID.3",
    "color": "white",
    "license_plate_number": "B-KK 402",
    "year": 2021
  },
  "payment_tx": "…",

  "address_index": 7,
  "script": "8303…",
  "payout_tx": "…"
}
```

| Field                         | Shown                                            |
| ----------------------------- | ------------------------------------------------ |
| `id` … `custodial`            | always                                           |
| `network`, `fare`, `pay_to`   | from `accepted`                                  |
| `fare_lovelace`               | from `accepted`, Cardano rides only              |
| `eta_minutes`, `eta_km`, `car` | to the driver from `accepted`; to the rider from `paid` |
| `payment_tx`                  | from `paid`                                      |
| `address_index`, `script`     | driver only, from `accepted`                     |
| `payout_tx`                   | driver only, from `paid_out`                     |

- `custodial`: `true` when the rider gave no `cosigner_key`.
- `network`: where the fare is paid, the accepting driver's chain
  (`cardano:*` or `bsv:*`); `fare` is in its smallest unit. `fare_lovelace`
  repeats a Cardano fare for older clients.
- `script`: on Cardano, hex CBOR of the native script locking `pay_to`, which
  the driver attaches to the payout transaction. On BSV the ride is locked by
  a bare 2-of-3 multisig output, which has no address: `pay_to` and `script`
  are both its hex locking script.
- `distance_km` is the straight-line distance between start and end.

The driver's public key is never shown to the rider.

---

## Rider endpoints

### `GET /ride` — quote a ride

Query parameters:

| Name            | Required | Description     |
| --------------- | -------- | --------------- |
| `start_address` | yes      | pickup address  |
| `end_address`   | yes      | drop-off address |
| `start`         | no       | `lat,lng` of the pickup; skips geocoding `start_address` |
| `end`           | no       | `lat,lng` of the drop-off; skips geocoding `end_address` |

```json
{
  "start_address": "Berlin Hbf",
  "end_address": "Alexanderplatz",
  "start": { "lat": 52.5251, "lng": 13.3694 },
  "end": { "lat": 52.5219, "lng": 13.4132 },
  "distance_km": 3.01,
  "fares": [
    { "network": "cardano:preprod", "min": 3505000, "max": 4200000 },
    { "network": "bsv:testnet", "min": 35000, "max": 35000 }
  ],
  "fare_lovelace": { "min": 3505000, "max": 4200000 }
}
```

`fares` holds the range over the registered drivers of each network, in its
smallest unit, and leaves out networks without drivers. `fare_lovelace`
repeats the Cardano range for older clients.

| Status | When                                   |
| ------ | -------------------------------------- |
| 200    | quoted                                 |
| 400    | an address parameter is missing, or bad `start`/`end` |
| 422    | an address could not be found          |
| 502    | the geocoder failed                    |

### `GET /search` — look up an address

Lists addresses matching free text, best first, so the rider can check which
place an address resolves to. `GET /ride` and `POST /ride` geocode an address
to its first match unless they are given its coordinates, so pass the `lat`
and `lng` of the result the rider picked.

Query parameters:

| Name    | Required | Description                     |
| ------- | -------- | ------------------------------- |
| `q`     | yes      | free-form address               |
| `limit` | no       | number of results, 1-10, default 5 |

```json
[
  { "address": "Alexanderplatz, 03238 Finsterwalde, Germany", "lat": 51.631357, "lng": 13.707521 },
  { "address": "Alexanderplatz, 10178 Berlin, Germany", "lat": 52.521666, "lng": 13.413191 }
]
```

| Status | When                                  |
| ------ | ------------------------------------- |
| 200    | list (possibly empty)                 |
| 400    | `q` missing or bad `limit`            |
| 502    | the geocoder failed                   |

### `POST /ride` — request a ride

```json
{
  "start_address": "Berlin Hbf",
  "end_address": "Alexanderplatz",
  "pickup_time": "2026-11-01T09:00:00Z",
  "cosigner_key": "addr_shared_vk1…",
  "start": { "lat": 52.5251, "lng": 13.3694 },
  "end": { "lat": 52.5219, "lng": 13.4132 }
}
```

`start` and `end` are optional. Each one given is used instead of geocoding
its address, which then only labels the ride.

`cosigner_key` is optional: the rider's public key for this ride, a Cardano
`addr_shared_vk1…` or a BSV compressed public key in hex. Account keys are
refused. Use a fresh key for every ride; a reused key links the rides on
chain. A Cardano key ties the ride to Cardano drivers, a BSV key to BSV
drivers. Without it the server takes the rider's place in the ride address,
the ride is `custodial`, and drivers on either chain can take it.

| Status | When                                                         |
| ------ | ------------------------------------------------------------ |
| 201    | created; body is the ride (`requested`)                      |
| 400    | missing fields, bad JSON, invalid `start`/`end`, a `cosigner_key` that is not a valid per-ride key (account keys included), or a BSV key while BSV is off |
| 422    | an address could not be found                                |
| 502    | the geocoder failed                                          |

### `GET /ride/{id}` — poll a ride, pay the fare

Free while the ride is `requested`, `paid` or `paid_out`. While it is
`accepted`, the fare must be paid once using
[x402](https://x402.org) v2 (`exact` scheme) on the ride's `network`:

1. Without payment the server answers `402` with the requirements, both in the
   body and base64-encoded in the `PAYMENT-REQUIRED` header. `accepts` holds
   one entry, for the ride's network:

   ```json
   {
     "x402Version": 2,
     "resource": { "url": "…/api/v1/ride/9f2c…", "description": "Kiko ride 9f2c…", "mimeType": "application/json" },
     "accepts": [{
       "scheme": "exact",
       "network": "cardano:preprod",
       "amount": "3505000",
       "asset": "lovelace",
       "payTo": "addr_test1w…",
       "maxTimeoutSeconds": 600
     }]
   }
   ```

   | Network     | `asset`    | `amount` in | `payTo`                                       |
   | ----------- | ---------- | ----------- | --------------------------------------------- |
   | `cardano:*` | `lovelace` | lovelace    | the ride's script address                     |
   | `bsv:*`     | `satoshi`  | satoshis    | the ride's locking script in hex (`5221…53ae`) |

2. The rider builds and signs a transaction paying `amount` to `payTo` (on
   BSV, an output with exactly that locking script), and retries with the
   `PAYMENT-SIGNATURE` header set to base64 of:

   ```json
   {
     "x402Version": 2,
     "accepted": { "…": "the entry from accepts" },
     "payload": {
       "transaction": "<base64 signed transaction>",
       "nonce": "<an output the transaction spends>"
     }
   }
   ```

   | Network     | `transaction`             | `nonce`              |
   | ----------- | ------------------------- | -------------------- |
   | `cardano:*` | Conway transaction CBOR   | `<txHash>#<index>`   |
   | `bsv:*`     | Extended Format (BRC-30)  | `<txid>:<vout>`      |

   On BSV the server's built-in facilitator executes every input script, so
   each input must carry the output it spends (Extended Format). It accepts
   the payment when one output pays exactly `amount` to `payTo` and the
   `nonce` is one of the inputs, and settles by broadcasting through ARC; it
   does not wait for a block.

3. The server verifies and settles the payment through the facilitator. On
   success it answers `200` with the ride (`paid`, including `car`) and a
   `PAYMENT-RESPONSE` header. Settlement can take minutes.

| Status | When                                                     |
| ------ | -------------------------------------------------------- |
| 200    | ride returned (or just paid)                             |
| 402    | payment required, invalid, or settlement failed; retry   |
| 404    | unknown ride                                             |
| 409    | another transaction is already paying this ride          |

---

## MCP server

The rider endpoints are also served as [MCP](https://modelcontextprotocol.io)
tools at `/mcp` (streamable HTTP, stateless), so an agent can book and pay for
a ride. Each tool runs the matching route above, with the same validation,
fares and payments.

| Tool             | Route             | Arguments                                                  |
| ---------------- | ----------------- | ---------------------------------------------------------- |
| `search_address` | `GET /search`     | `q`, `limit`                                               |
| `quote_ride`     | `GET /ride`       | `start_address`, `end_address`, `start`, `end` (`{lat, lng}`) |
| `request_ride`   | `POST /ride`      | the request body                                           |
| `get_ride`       | `GET /ride/{id}`  | `id`, `payment_signature`                                  |

A tool returns the route's JSON body. From status 400 on the result is an
error whose text is `"<status> <reason>: <body>"`. So while a ride is
`accepted`, `get_ride` fails with `402 Payment Required:` and the x402
requirements; calling it again with `payment_signature` (the base64
`PAYMENT-SIGNATURE` value) pays the fare. A paid result has a second text
content, `x402 settlement: <PAYMENT-RESPONSE JSON>`.

Each ride's `payTo` is its own fresh [ride address](#ride-address-2-of-3-multisig),
so riders' payments cannot be linked on chain. The server instructions and the
`get_ride` description tell agents to expect a new, unused address on every
ride and to check the ride id in the requirements' description and the amount
instead.

They also spell out how paying on `bsv:*` differs from usual x402: `payTo` is
a hex locking script to lock an output with, that output pays exactly
`amount`, the transaction is Extended Format (BRC-30) rather than raw, the
`nonce` is `<txid>:<vout>`, and the ride is paid once ARC accepts the
transaction.

---

## Driver endpoints

Drivers are identified by their public key, and every driver request,
registration included, is signed with the matching private key:

```
X-Driver-Key: acct_shared_xvk1… or xpub…   (not on registration)
X-Driver-Timestamp: 1793386002
X-Driver-Signature: 5f3a…                   (hex)
```

- `X-Driver-Timestamp` is the Unix time in seconds. It must be within 30
  seconds of the server's clock, in either direction.
- `X-Driver-Signature` is made with the account private key behind
  `public_key` itself, not one derived from it, over these bytes:

  ```
  kiko-driver-request\n<timestamp>\n<METHOD>\n<path and query>\n<raw body>
  ```

  | Chain   | Account key                          | Signature                               |
  | ------- | ------------------------------------ | --------------------------------------- |
  | Cardano | CIP-1854 `m/1854'/1815'/account'`    | Ed25519 over the bytes, 64 bytes        |
  | BSV     | BIP-32 `m/48'/236'/account'`         | ECDSA secp256k1 over their SHA-256, DER |

  `<path and query>` is the request target exactly as sent, e.g.
  `/api/v1/driver/drive/available?location=52.52,13.37`. The body is the exact
  bytes sent, empty for `GET`. In Go, `api.DriverMessage` builds the message
  and `multisig.Signer.SignMessage` or `bsvmultisig.Signer.SignMessage` signs
  it.

On registration the signature is checked against `public_key` in the body;
everywhere else against `X-Driver-Key`, which must name a registered driver.
A missing, stale or wrong signature answers `401`. A signature can be replayed
for the same request within its 30 seconds, so send requests over HTTPS.

### `POST /driver` — register or update

```json
{
  "public_key": "acct_shared_xvk1…",
  "base_fee": 2000000,
  "fee_per_km": 500000,
  "car": {
    "brand": "VW",
    "model": "ID.3",
    "color": "white",
    "license_plate_number": "B-KK 402",
    "year": 2021
  }
}
```

`car.year` is the model year, from 1900 to next year.

`public_key` identifies the driver, cosigns each of their rides and decides
the chain they are paid on:

| `public_key`                       | Chain   | `base_fee`, `fee_per_km` in | Minimum fare      |
| ---------------------------------- | ------- | --------------------------- | ----------------- |
| CIP-1854 `acct_shared_xvk1…`       | Cardano | lovelace                    | 1,000,000         |
| BIP-32 account key `xpub…`/`tpub…` at `m/48'/236'/account'` | BSV | satoshis   | 1                 |

The fare is `max(base_fee + ceil(fee_per_km × distance_km), minimum)`. The
response adds the driver's `network`.

Posting again with a registered `public_key` replaces that driver's fees and
car with the new values. Rides the driver has already accepted keep the fare
fixed when they accepted.

| Status | When                                                          |
| ------ | ------------------------------------------------------------- |
| 201    | registered; body echoes the driver                            |
| 200    | already registered; profile replaced, body echoes the driver   |
| 400    | bad JSON, invalid or server-owned `public_key`, a BSV key while BSV is off, car fields missing, or `year` out of range |
| 401    | missing, stale or wrong signature by `public_key`              |

### `GET /driver/drive/available` — find rides

Query parameters:

| Name       | Required | Description                         |
| ---------- | -------- | ----------------------------------- |
| `location` | yes      | `lat,lng` of the driver             |
| `range`    | no       | search radius in km, default `5`    |

Returns the `requested` rides the driver can take whose pickup is within
range, nearest first: custodial rides, and rides whose rider key is on the
driver's chain.

```json
[
  {
    "id": "9f2c…",
    "start_address": "Berlin Hbf",
    "end_address": "Alexanderplatz",
    "start": { "lat": 52.5251, "lng": 13.3694 },
    "end": { "lat": 52.5219, "lng": 13.4132 },
    "distance_km": 3.01,
    "pickup_distance_km": 0.4,
    "pickup_time": "2026-11-01T09:00:00Z"
  }
]
```

| Status | When                                  |
| ------ | ------------------------------------- |
| 200    | list (possibly empty)                 |
| 400    | bad `location` or `range`             |
| 401    | missing or unknown `X-Driver-Key`, or bad signature |

### `POST /driver/drive/{ride_id}` — accept a ride

```json
{ "eta_minutes": 4, "eta_km": 1.2 }
```

Fixes the fare from the driver's rates and derives the ride's `pay_to`
address. Returns the ride (`accepted`) with the driver-only fields
`address_index` and `script`.

| Status | When                                                         |
| ------ | ------------------------------------------------------------ |
| 200    | accepted                                                     |
| 400    | bad JSON or negative ETA                                     |
| 401    | missing or unknown `X-Driver-Key`, or bad signature |
| 404    | unknown ride                                                 |
| 409    | ride already taken                                           |
| 422    | the rider's key is on another chain, or the ride address cannot be formed (the rider's key is the driver's or the server's at the ride's index) |

### `GET /driver/drive/{ride_id}` — view an accepted ride

Returns the ride with driver-only fields. Only the driver who accepted it can
see it.

| Status | When                                       |
| ------ | ------------------------------------------ |
| 200    | ride returned                              |
| 401    | missing or unknown `X-Driver-Key`, or bad signature |
| 404    | unknown ride or accepted by another driver |

### `POST /driver/drive/{ride_id}/payout` — get paid

Once the ride is `paid`, the driver builds a transaction.

On **Cardano**, a Conway transaction that

- spends the UTxOs at the ride's `pay_to` address,
- attaches the ride's `script` as a native script witness,
- pays a fee that covers **two** vkey witnesses (the driver's and the
  server's), and
- is signed by the driver's payment key at `m/1854'/1815'/account'/0/address_index`,

and posts it:

```json
{ "transaction": "<hex transaction CBOR, signed by the driver>" }
```

The transaction body is not changed, so its id is the one the driver signed.

On **BSV**, a transaction in Extended Format (hex) whose every input spends
the ride's locking script and is unlocked by `OP_0 <driver signature>`,
signed `SIGHASH_ALL|FORKID` with the driver's key at
`m/48'/236'/account'/0/address_index`, with a fee that covers the server's
signature too. The server's signature completes the unlocking script, which
changes the transaction id.

The server checks the driver's signature, adds its own, and submits the
transaction. On success it returns the ride (`paid_out`) with `payout_tx`,
the submitted transaction's id.

| Status | When                                                              |
| ------ | ----------------------------------------------------------------- |
| 200    | submitted; ride is `paid_out`                                     |
| 400    | bad JSON or `transaction` is not hex                              |
| 401    | missing or unknown `X-Driver-Key`, or bad signature |
| 404    | unknown ride or accepted by another driver                        |
| 409    | ride is not `paid`, already `paid_out`, or a payout is in progress |
| 422    | not signed by the driver, wrong script, or an invalid signature   |
| 502    | the chain rejected the transaction; the ride stays `paid`, fix and retry |
