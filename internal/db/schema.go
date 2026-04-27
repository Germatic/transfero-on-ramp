package db

const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Locked Transfero quote sessions, kept until confirmed or expired.
CREATE TABLE IF NOT EXISTS onramp_quotes (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id           TEXT        NOT NULL,            -- maps to dinacore merchantId
    transfero_session_id TEXT        NOT NULL,
    brl_amount           NUMERIC(20,6) NOT NULL,
    usdt_amount          NUMERIC(20,6) NOT NULL,
    price                NUMERIC(20,6) NOT NULL,          -- BRL per USDT
    settlement           TEXT        NOT NULL,            -- D0 | D1 | D2
    destination_address  TEXT,                            -- Tron address (provided at confirm time, stored here for reference)
    network              TEXT        NOT NULL DEFAULT 'mainnet',
    status               TEXT        NOT NULL DEFAULT 'open',  -- open | used | expired
    expires_at           TIMESTAMPTZ NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- make destination_address nullable for existing tables (idempotent)
ALTER TABLE onramp_quotes ALTER COLUMN destination_address DROP NOT NULL;

CREATE INDEX IF NOT EXISTS onramp_quotes_status_expires
    ON onramp_quotes (status, expires_at);
CREATE INDEX IF NOT EXISTS onramp_quotes_account_id
    ON onramp_quotes (account_id);

-- Confirmed on-ramp orders (one per closed Transfero session).
CREATE TABLE IF NOT EXISTS onramp_orders (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id           TEXT        NOT NULL,
    quote_id             UUID        NOT NULL REFERENCES onramp_quotes(id),
    transfero_closing_id TEXT        NOT NULL,
    oid                  TEXT        NOT NULL UNIQUE,     -- idempotency key (= quote_id)
    brl_amount           NUMERIC(20,6) NOT NULL,
    usdt_amount          NUMERIC(20,6) NOT NULL,
    price                NUMERIC(20,6) NOT NULL,
    settlement           TEXT        NOT NULL,
    destination_address  TEXT        NOT NULL,
    network              TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'confirmed',  -- awaiting_settlement | confirmed | delivering | delivered | failed | payment_failed
    pix_payment_group_id TEXT,       -- Transfero paymentGroupId for the BRL PIX sent to OTC desk
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- add pix_payment_group_id if table already exists (idempotent migration)
ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS pix_payment_group_id TEXT;

-- fee audit columns (idempotent)
ALTER TABLE onramp_quotes ADD COLUMN IF NOT EXISTS fee_pct   NUMERIC(8,6) NOT NULL DEFAULT 0;
ALTER TABLE onramp_quotes ADD COLUMN IF NOT EXISTS raw_price NUMERIC(20,6);

ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS fee_pct   NUMERIC(8,6) NOT NULL DEFAULT 0;
ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS raw_price NUMERIC(20,6);

CREATE INDEX IF NOT EXISTS onramp_orders_account_id ON onramp_orders (account_id);
CREATE INDEX IF NOT EXISTS onramp_orders_quote_id   ON onramp_orders (quote_id);
CREATE INDEX IF NOT EXISTS onramp_orders_status     ON onramp_orders (status);

-- -------------------------------------------------------
-- Onramp fee schedule — per account + currency pair.
-- fee_pct is a multiplier applied to Transfero's raw price
-- at quote time: adjusted_price = raw_price * (1 + fee_pct).
-- e.g. 0.002000 = 0.2% markup. 0 = passthrough (default).
-- effective_from PK preserves full audit history; the latest
-- row per (account_id, from_currency, to_currency) is active.
-- No row = 0% fee.
-- -------------------------------------------------------
CREATE TABLE IF NOT EXISTS onramp_fees (
  account_id     TEXT         NOT NULL,
  from_currency  TEXT         NOT NULL,
  to_currency    TEXT         NOT NULL,
  fee_pct        NUMERIC(8,6) NOT NULL DEFAULT 0,
  effective_from TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, from_currency, to_currency, effective_from)
);

CREATE INDEX IF NOT EXISTS onramp_fees_lookup
  ON onramp_fees (account_id, from_currency, to_currency, effective_from DESC);

-- -------------------------------------------------------
-- Per-account on-ramp settings.
-- max_d0_premium_pct: maximum allowed percentage by which the D0 settlement
--   price may exceed the spot price before the trade is rejected.
--   NULL = no guard (all prices accepted).
--   e.g. 0.036000 = 0.036%.
-- -------------------------------------------------------
CREATE TABLE IF NOT EXISTS onramp_account_settings (
  account_id           TEXT        PRIMARY KEY,
  max_d0_premium_pct   NUMERIC(10,6),               -- NULL = disabled
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
`
