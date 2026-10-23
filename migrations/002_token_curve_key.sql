-- Remember each token's bonding curve address (needed to fetch its history over RPC).
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS bonding_curve_key TEXT NOT NULL DEFAULT '';
