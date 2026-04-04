# transfero-on-ramp

Adapter service that converts BRL balances held in Dinaria/DinaCore into USDT
delivered on-chain to a Tron address, via the **Transfero OTC API**.

## How it fits into the system

```
Customer ──Bearer token──► transfero-on-ramp ──────────────────────────────────┐
                                │                                               │
                                ├─ resolve key ──► dinapay DB (api_keys)        │
                                │                                               │
                                ├─ debit BRL ────► dinacore  (/api/balance/debit)
                                │                                               │
                                ├─ lock price ───► Transfero (/v1/sessions)     │
                                │                                               │
                                ├─ confirm ──────► Transfero (/v1/sessions/:id/close)
                                │                                               │
                                └─ credit USDT ──► dinacore  (/api/balance/credit)
                                                                                │
                              Transfero delivers USDT on-chain ─────────────────┘
```

## API

All protected endpoints require `Authorization: Bearer <key>`.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Liveness probe |
| POST | `/v1/quotes` | Request a BRL→USDT price quote (locks price for ~7 s) |
| POST | `/v1/orders` | Confirm a quote and execute the trade |
| GET | `/v1/orders` | List orders (`?page=1&pageSize=50`) |
| GET | `/v1/orders/{id}` | Get a single order |

### POST /v1/quotes

```json
{
  "brlAmount": 25000.00,
  "destinationAddress": "TXyz...",
  "settlement": "D0",
  "network": "mainnet"
}
```

Response `201`:
```json
{
  "quoteId": "uuid",
  "usdtAmount": 4432.18,
  "brlAmount": 25000.00,
  "price": 5.64,
  "settlement": "D0",
  "destinationAddress": "TXyz...",
  "network": "mainnet",
  "expiresAt": "2026-04-03T12:00:07Z"
}
```

### POST /v1/orders

```json
{ "quoteId": "uuid" }
```

Response `201`:
```json
{
  "orderId": "uuid",
  "quoteId": "uuid",
  "closingId": "transfero-closing-id",
  "usdtAmount": 4432.18,
  "brlAmount": 25000.00,
  "price": 5.64,
  "settlement": "D0",
  "destinationAddress": "TXyz...",
  "network": "mainnet",
  "status": "confirmed",
  "createdAt": "2026-04-03T12:00:07Z"
}
```

## Running locally

```bash
cp .env.example .env
# edit .env

go run ./cmd/onramp
```

## Building

```bash
go build -o bin/onramp ./cmd/onramp
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `ONRAMP_PORT` | `8094` | HTTP listen port |
| `ONRAMP_DB_URL` | — | PostgreSQL DSN for this service |
| `DINAPAY_DB_URL` | — | Dinapay PostgreSQL DSN (API key resolution) |
| `DINACORE_URL` | `http://localhost:8093` | DinaCore balance API base URL |
| `DINACORE_API_KEY` | — | DinaCore API key |
| `TRANSFERO_API_URL` | `https://staging.otc.transfero.com` | Transfero OTC base URL |
| `TRANSFERO_API_KEY` | — | Transfero API key |
| `ONRAMP_API_KEYS` | — | Comma-separated static fallback keys |

## Failure handling

If the `POST /v1/sessions/:id/close` call to Transfero fails **after** BRL has
already been debited from DinaCore, the service:

1. Queries `GET /v1/closings` for an entry whose `oid` matches the `quoteId`.
2. **Trade found** → credits USDT and persists the order normally.
3. **Trade not found** → refunds BRL via `POST /api/balance/credit` and returns
   an error to the caller (safe to retry with the same `quoteId`).

Both the debit and the refund are logged with `slog` at `ERROR` level so they
can be caught by alerting if the DinaCore calls themselves fail.
