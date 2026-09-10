-- +goose Up

-- The account on the network that a participant's tokens are actually delivered to.
--
-- A wallet address is not one. On Hedera a token cannot be sent to an address that has
-- never been an account, and the receiving side must have associated the token or have an
-- automatic association slot free — so an investor that signed in with a browser wallet has
-- an address the platform can verify a signature against and nothing it can transfer to.
--
-- The column is empty for a participant that has no account yet, and settlement falls back
-- to the wallet address, which is what the in-process ledger uses. Filling it is a
-- deliberate act with a cost on a real network, so it is done by a command rather than by a
-- request handler.
ALTER TABLE organizations ADD COLUMN chain_account_id TEXT NOT NULL DEFAULT '';

-- One account belongs to one participant. Two organizations sharing an account would make
-- every balance on the network ambiguous about who holds what.
CREATE UNIQUE INDEX organizations_chain_account_key
    ON organizations (chain_account_id)
    WHERE chain_account_id <> '';

-- +goose Down
DROP INDEX IF EXISTS organizations_chain_account_key;
ALTER TABLE organizations DROP COLUMN IF EXISTS chain_account_id;
