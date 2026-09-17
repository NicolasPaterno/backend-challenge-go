-- §11 owes a WagerTransactionRejected to every rejection, which needs a row to
-- emit from. A bet naming a wallet that does not exist is refused with
-- WALLET_NOT_FOUND, and the foreign key is what stopped that refusal being
-- recorded, so it goes. The column stays NOT NULL: the requested wallet id is
-- the point of the audit row.
--
-- No financial invariant rests on this key. Money moves only through
-- wallet_ledger_entries, whose own reference to wallets is untouched, so a
-- transaction naming an absent wallet can never carry an entry.
ALTER TABLE wager_transactions DROP CONSTRAINT wager_transactions_wallet_id_fkey;
