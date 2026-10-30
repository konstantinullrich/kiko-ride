# Kiko API endpoints

All endpoints are under `/api/v1`, take and return JSON, and report errors as

```json
{ "error": "human readable message" }
```

Amounts are in **lovelace** (1 ADA = 1,000,000 lovelace). Times are RFC 3339.
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

Each ride is paid to its own native-script address that any two of three keys
can spend:

| Key    | Source                                                             |
| ------ | ------------------------------------------------------------------ |
| Server | Kiko's server key                                                  |
| Driver | The driver's `public_key`                                          |
| Rider  | The rider's `cosigner_key`, or a second server key if none is given |

All keys are CIP-1854 account keys (`acct_shared_xvk1…`); each party's key for
a ride is derived at the ride's `address_index` (role 0). If the rider gives no
key the ride is `custodial` and the server holds two of the three keys.

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
| `fare_lovelace`, `pay_to`     | from `accepted`                                  |
| `eta_minutes`, `eta_km`, `car` | to the driver from `accepted`; to the rider from `paid` |
| `payment_tx`                  | from `paid`                                      |
| `address_index`, `script`     | driver only, from `accepted`                     |
| `payout_tx`                   | driver only, from `paid_out`                     |

- `custodial`: `true` when the rider gave no `cosigner_key`.
- `script`: hex CBOR of the native script locking `pay_to`. The driver attaches
  it to the payout transaction.
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
  "fare_lovelace": { "min": 3505000, "max": 4200000 }
}
```

`fare_lovelace` is the range over all registered drivers and is omitted when
none are registered.

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
  "cosigner_key": "acct_shared_xvk1…",
  "start": { "lat": 52.5251, "lng": 13.3694 },
  "end": { "lat": 52.5219, "lng": 13.4132 }
}
```

`start` and `end` are optional. Each one given is used instead of geocoding
its address, which then only labels the ride.

`cosigner_key` is optional. Without it the server takes the rider's place in
the ride address and the ride is `custodial`.

| Status | When                                                         |
| ------ | ------------------------------------------------------------ |
| 201    | created; body is the ride (`requested`)                      |
| 400    | missing fields, bad JSON, invalid `start`/`end`, or an invalid or server-owned `cosigner_key` |
| 422    | an address could not be found                                |
| 502    | the geocoder failed                                          |

### `GET /ride/{id}` — poll a ride, pay the fare

Free while the ride is `requested`, `paid` or `paid_out`. While it is
`accepted`, the fare must be paid once using
[x402](https://x402.org) v2 (`exact` scheme, Cardano):

1. Without payment the server answers `402` with the requirements, both in the
   body and base64-encoded in the `PAYMENT-REQUIRED` header:

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

2. The rider builds and signs a transaction paying `amount` to `payTo`, and
   retries with the `PAYMENT-SIGNATURE` header set to base64 of:

   ```json
   {
     "x402Version": 2,
     "accepted": { "…": "the entry from accepts" },
     "payload": {
       "transaction": "<base64 signed transaction CBOR>",
       "nonce": "<txHash>#<index of a UTxO the transaction spends>"
     }
   }
   ```

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

## Driver endpoints

Drivers are identified by their public key. Every driver endpoint except
registration needs the header

```
X-Driver-Key: acct_shared_xvk1…
```

and answers `401` if it does not name a registered driver. The key is a bearer
credential: keep it private.

### `POST /driver` — register

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

`public_key` is the driver's CIP-1854 account public key. It identifies the
driver and cosigns each of their rides. The fare is
`max(base_fee + ceil(fee_per_km × distance_km), 1000000)`.

| Status | When                                                          |
| ------ | ------------------------------------------------------------- |
| 201    | registered; body echoes the driver                            |
| 400    | bad JSON, invalid or server-owned `public_key`, car fields missing, or `year` out of range |
| 409    | a driver with this `public_key` is already registered         |

### `GET /driver/drive/available` — find rides

Query parameters:

| Name       | Required | Description                         |
| ---------- | -------- | ----------------------------------- |
| `location` | yes      | `lat,lng` of the driver             |
| `range`    | no       | search radius in km, default `5`    |

Returns the `requested` rides whose pickup is within range, nearest first:

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
| 401    | missing or unknown `X-Driver-Key`     |

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
| 401    | missing or unknown `X-Driver-Key`                            |
| 404    | unknown ride                                                 |
| 409    | ride already taken                                           |
| 422    | the ride address cannot be formed (rider uses the driver's key) |

### `GET /driver/drive/{ride_id}` — view an accepted ride

Returns the ride with driver-only fields. Only the driver who accepted it can
see it.

| Status | When                                       |
| ------ | ------------------------------------------ |
| 200    | ride returned                              |
| 401    | missing or unknown `X-Driver-Key`          |
| 404    | unknown ride or accepted by another driver |

### `POST /driver/drive/{ride_id}/payout` — get paid

Once the ride is `paid`, the driver builds a Conway transaction that

- spends the UTxOs at the ride's `pay_to` address,
- attaches the ride's `script` as a native script witness,
- pays a fee that covers **two** vkey witnesses (the driver's and the
  server's), and
- is signed by the driver's payment key at `m/1854'/1815'/account'/0/address_index`,

and posts it:

```json
{ "transaction": "<hex transaction CBOR, signed by the driver>" }
```

The server checks the driver's signature, adds its own, and submits the
transaction. The transaction body is not changed, so its id is the one the
driver signed. On success it returns the ride (`paid_out`) with `payout_tx`.

| Status | When                                                              |
| ------ | ----------------------------------------------------------------- |
| 200    | submitted; ride is `paid_out`                                     |
| 400    | bad JSON or `transaction` is not hex                              |
| 401    | missing or unknown `X-Driver-Key`                                 |
| 404    | unknown ride or accepted by another driver                        |
| 409    | ride is not `paid`, already `paid_out`, or a payout is in progress |
| 422    | not signed by the driver, wrong script, or an invalid signature   |
| 502    | the chain rejected the transaction; the ride stays `paid`, fix and retry |
