-- +goose Up

-- A machine customer pays in what the network moves, and that unit is not always three
-- letters. CHAR(3) was an ISO 4217 assumption that quietly held everywhere a price is
-- stored; it is right for an invoice, which is denominated in a currency, and wrong for a
-- paid request, which is denominated in whatever the payer's chain uses.
--
-- Only this table changes. A receivable's face value stays a currency, because that is what
-- a debtor owes.
ALTER TABLE x402_requests ALTER COLUMN currency TYPE TEXT;

ALTER TABLE x402_requests ADD CONSTRAINT x402_requests_currency_present
    CHECK (length(btrim(currency)) BETWEEN 3 AND 8);

-- +goose Down
ALTER TABLE x402_requests DROP CONSTRAINT IF EXISTS x402_requests_currency_present;
ALTER TABLE x402_requests ALTER COLUMN currency TYPE CHAR(3) USING left(currency, 3);
