-- §7 asks for two things and this index carries both. "Uma referência não
-- recebe duas reversões bem-sucedidas do mesmo tipo" is the weaker half; the
-- stronger is "impedindo devolução duplicada do mesmo débito", which a
-- per-kind index would miss, because a REFUND and a ROLLBACK are different
-- kinds and together return one debit twice (A.8.1).
--
-- So the key is the reference alone: one successful reversal spends it.
-- Undoing a refund is a ROLLBACK of the REFUND, which has its own slot here,
-- which is why §7 lists REFUND as a rollback target.
CREATE UNIQUE INDEX wager_transactions_reference_reversal_unique
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');
