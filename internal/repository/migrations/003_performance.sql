-- 003_performance.sql - indexes, tenant-scoped lookups and ledger invariants.
--
-- Every index below maps to a specific query that was doing a sequential scan:
--
--   idx_journal_lines_account        ListAccounts / GetAccount / GetAccountByCode
--                                    / ListLedgerEntries all join
--                                    journal_lines l ON l.account_id = a.id, and
--                                    only idx_journal_lines_entry existed, so
--                                    every account list, dashboard and report
--                                    full-scanned the line table. This is the
--                                    single largest gap.
--   idx_journal_entries_company_number  ListJournalEntries: WHERE company_id = $1
--                                    [AND status = $2] ORDER BY number DESC.
--   idx_invoices_company_created    ListInvoices: WHERE company_id = $1
--                                    ORDER BY created_at DESC, number DESC.
--   idx_audit_logs_company_created  ListAuditLogs: WHERE company_id = $1
--                                    ORDER BY created_at DESC, id DESC.
--   idx_branches_company            ListBranches / resolveBranch.
--   idx_customers_company           ListCustomers.
--   idx_users_company               company-scoped user lookups.
--   idx_users_email_lower           FindUserByEmail: LOWER(email) = LOWER($1).
--                                    An index on the raw column cannot serve a
--                                    LOWER() predicate, so the previous
--                                    idx_users_email was dead weight for login.
--   idx_password_reset_tokens_expiry  sweeping spent/expired reset tokens.
--
-- The CHECK constraints encode invariants the service layer already enforces,
-- so existing data satisfies them. They are added as a backstop: a bug or a
-- manual fix in psql would otherwise be able to write a negative or
-- double-sided journal line straight into the ledger. Each is guarded by a
-- pg_constraint lookup so re-running against a database that already has it
-- cannot fail. They run inline (not NOT VALID) because the tables are small and
-- a full validation pass is cheaper than maintaining a second code path.
--
-- Everything is IF NOT EXISTS: this file runs inside the migration transaction
-- applied by MigrateTables, and must stay safe to apply to a database that has
-- partially run it.

CREATE INDEX IF NOT EXISTS idx_journal_lines_account ON journal_lines(account_id);
CREATE INDEX IF NOT EXISTS idx_journal_entries_company_number ON journal_entries(company_id, number DESC);
CREATE INDEX IF NOT EXISTS idx_invoices_company_created ON invoices(company_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_company_created ON audit_logs(company_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_branches_company ON branches(company_id);
CREATE INDEX IF NOT EXISTS idx_customers_company ON customers(company_id);
CREATE INDEX IF NOT EXISTS idx_users_company ON users(company_id);
CREATE INDEX IF NOT EXISTS idx_users_email_lower ON users(LOWER(email));
CREATE INDEX IF NOT EXISTS idx_password_reset_tokens_expiry ON password_reset_tokens(expires_at);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_journal_lines_debit_non_negative'
    ) THEN
        ALTER TABLE journal_lines
            ADD CONSTRAINT chk_journal_lines_debit_non_negative CHECK (debit >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_journal_lines_credit_non_negative'
    ) THEN
        ALTER TABLE journal_lines
            ADD CONSTRAINT chk_journal_lines_credit_non_negative CHECK (credit >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_journal_lines_one_sided'
    ) THEN
        ALTER TABLE journal_lines
            ADD CONSTRAINT chk_journal_lines_one_sided
            CHECK (NOT (debit > 0 AND credit > 0));
    END IF;
END
$$;

-- accounts.balance is a legacy cache that nothing reads: every balance in the
-- product (account list, ledger, balance sheet, dashboard) is computed from
-- posted journal lines, which is what keeps the trial balance reconciling after
-- a reseed. The column stays because it is NOT NULL in deployed databases and
-- CreateAccount still writes 0 to it; it must not be trusted as a source of
-- truth, and this note is the only thing standing between a future reader and a
-- report that silently disagrees with the ledger.
COMMENT ON COLUMN accounts.balance IS
    'Legacy/unused cache. All balances are computed from posted journal_lines; do not read this column as a source of truth.';