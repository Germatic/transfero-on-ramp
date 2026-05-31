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

-- tx_hash: on-chain Tron transaction hash set by the settlement reconciler
ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS tx_hash TEXT;
-- delivered_at: timestamp when on-chain delivery was confirmed
ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ;
-- payout_id: foreign key back to the dinapay payouts table row
ALTER TABLE onramp_orders ADD COLUMN IF NOT EXISTS payout_id TEXT;

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
-- onramp_account_settings — per-account BRL→USDT pricing knobs.
--
-- The customer-facing price follows a two-regime rule, anchored on the
-- (Spot, D0) pair Transfero returns from a live session:
--
--   basis = D0 / Spot                      -- how much premium Transfero
--                                          -- bakes into the settlement leg
--
--   if basis ≤ 1 + basis_threshold_pct/100   (low-basis regime)
--      customer_price = Spot × (1 + spot_markup_pct/100)
--   else                                      (high-basis regime)
--      customer_price = D0   × (1 + d0_markup_pct/100)
--
-- Defaults match the operator spec landed 2026-05-28:
--   spot_markup_pct      = 0.36   → 36 bps over Spot in the low-basis regime
--   d0_markup_pct        = 0.20   → 20 bps over D0   in the high-basis regime
--   basis_threshold_pct  = 0.25   → regime flips at D0/Spot = 1.0025
--
-- The high-basis branch replaces the old MARKET_DISLOCATION 422 reject:
-- every quote now executes; the operator's margin in the high-basis regime
-- is guaranteed at d0_markup_pct of D0 instead of zero / blocked.
--
-- Every account that uses the on-ramp MUST have a row here.
-- i.e. 0.36 means zero point 36 percent (0.36 %).
-- -------------------------------------------------------
CREATE TABLE IF NOT EXISTS onramp_account_settings (
  account_id          TEXT          PRIMARY KEY,
  spot_markup_pct     NUMERIC(10,6) NOT NULL,                   -- e.g. 0.360000 = 0.36 %
  d0_markup_pct       NUMERIC(10,6) NOT NULL DEFAULT 0.200000,  -- high-basis markup
  basis_threshold_pct NUMERIC(10,6) NOT NULL DEFAULT 0.250000,  -- regime cutover at D0/Spot
  description         TEXT,
  updated_at          TIMESTAMPTZ   NOT NULL DEFAULT now()
);
ALTER TABLE onramp_account_settings ADD COLUMN IF NOT EXISTS description         TEXT;
ALTER TABLE onramp_account_settings ADD COLUMN IF NOT EXISTS spot_markup_pct     NUMERIC(10,6);
ALTER TABLE onramp_account_settings ADD COLUMN IF NOT EXISTS d0_markup_pct       NUMERIC(10,6) NOT NULL DEFAULT 0.200000;
ALTER TABLE onramp_account_settings ADD COLUMN IF NOT EXISTS basis_threshold_pct NUMERIC(10,6) NOT NULL DEFAULT 0.250000;
ALTER TABLE onramp_account_settings DROP COLUMN IF EXISTS   max_d0_premium_pct;

-- Sanity bounds: a fat-finger UPDATE can't silently make us 1000-bps cheap or
-- expensive. Drop-then-add keeps EnsureSchema idempotent across boots.
ALTER TABLE onramp_account_settings
  DROP CONSTRAINT IF EXISTS onramp_account_settings_spot_markup_chk,
  ADD  CONSTRAINT          onramp_account_settings_spot_markup_chk
       CHECK (spot_markup_pct >= 0 AND spot_markup_pct <= 10);
ALTER TABLE onramp_account_settings
  DROP CONSTRAINT IF EXISTS onramp_account_settings_d0_markup_chk,
  ADD  CONSTRAINT          onramp_account_settings_d0_markup_chk
       CHECK (d0_markup_pct >= 0 AND d0_markup_pct <= 10);
ALTER TABLE onramp_account_settings
  DROP CONSTRAINT IF EXISTS onramp_account_settings_basis_threshold_chk,
  ADD  CONSTRAINT          onramp_account_settings_basis_threshold_chk
       CHECK (basis_threshold_pct >= 0 AND basis_threshold_pct <= 100);
`
