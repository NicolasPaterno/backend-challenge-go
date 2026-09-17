-- The rejections this migration allowed name no existing wallet, so the key
-- cannot be restored while they are stored.
DELETE FROM wager_transactions
WHERE wallet_id NOT IN (SELECT id FROM wallets);

ALTER TABLE wager_transactions
    ADD CONSTRAINT wager_transactions_wallet_id_fkey
    FOREIGN KEY (wallet_id) REFERENCES wallets (id);
