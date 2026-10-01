package repository

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"tera/internal/domain"
)

// PostgresStore implements Store backed by PostgreSQL.
type PostgresStore struct {
	pool *pgxpool.Pool
	db   postgresDB
}

type postgresDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

// DatabaseURLFromEnv reports the configured PostgreSQL connection string, or an
// empty string when the application should fall back to the in-memory store.
func DatabaseURLFromEnv() string {
	return strings.TrimSpace(os.Getenv("DATABASE_URL"))
}

// wipeTarget is one table the demo reset has to clear and the column that
// scopes it.
type wipeTarget struct {
	table string
	// parent/parentColumn scope rows by a parent document, for the line tables
	// that carry no company_id of their own.
	parent       string
	parentColumn string
	// key scopes rows directly; it defaults to company_id, and the tenant table
	// itself is keyed on its own id.
	key string
}

// demoWipeTargets lists the tables a demo reset must clear, in dependency order.
// Children come before parents because several foreign keys are ON DELETE
// RESTRICT (invoice_payments -> invoices, journal_lines -> accounts, both
// documents -> branches): deleting a parent first would abort the transaction
// and leave the tenant half-wiped.
var demoWipeTargets = []wipeTarget{
	{table: "invoice_lines", parent: "invoices", parentColumn: "invoice_id"},
	{table: "journal_lines", parent: "journal_entries", parentColumn: "journal_entry_id"},
	{table: "audit_logs"},
	{table: "invoice_payments"},
	{table: "sessions"},
	{table: "password_reset_tokens"},
	{table: "number_sequences"},
	{table: "customers"},
	{table: "invoices"},
	{table: "journal_entries"},
	{table: "accounts"},
	{table: "branches"},
	{table: "users"},
	{table: "companies", key: "id"},
}

// WipeDemoTenant deletes the demo tenant and every row that belongs to it.
//
// Seeding is skipped when the demo user already exists (so a normal restart
// never duplicates data), which means a deployment initialised with an older
// dataset keeps serving that stale data. This lets an operator deliberately
// reset the demo tenant so the current seed dataset can be applied.
//
// Everything happens in one transaction, and the tables it touches are resolved
// from information_schema first. The previous version named three tables that
// have never existed (ledger_entries, period_locks, tax_transactions) and hid
// every failure behind a string match on "does not exist", so a delete that
// failed for any other reason - a lock, a permission problem, a constraint -
// was silently skipped and the reset reported success.
func (p *PostgresStore) WipeDemoTenant(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin demo wipe: %w", err)
	}
	defer tx.Rollback(ctx)

	companyID, err := demoCompanyID(ctx, tx)
	if err != nil {
		return err
	}
	if companyID == 0 {
		return tx.Commit(ctx)
	}

	present, err := existingColumns(ctx, tx)
	if err != nil {
		return err
	}
	for _, target := range demoWipeTargets {
		query, ok := target.deleteQuery(companyID, present)
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, query, companyID); err != nil {
			return fmt.Errorf("wipe %s: %w", target.table, err)
		}
	}
	return tx.Commit(ctx)
}

// deleteQuery builds the DELETE for one target, or reports false when this
// deployment does not have the table (or the column that scopes it), so an
// older schema is reset as completely as it can be instead of failing.
func (target wipeTarget) deleteQuery(companyID uint, present map[string]bool) (string, bool) {
	if !present[target.table] {
		return "", false
	}
	if target.parent != "" {
		if !present[target.parent] || !present[target.parent+".company_id"] {
			return "", false
		}
		if !present[target.table+"."+target.parentColumn] {
			return "", false
		}
		return fmt.Sprintf(
			"DELETE FROM %s WHERE %s IN (SELECT id FROM %s WHERE company_id = $1)",
			target.table, target.parentColumn, target.parent,
		), true
	}
	key := target.key
	if key == "" {
		key = "company_id"
	}
	if !present[target.table+"."+key] {
		return "", false
	}
	return fmt.Sprintf("DELETE FROM %s WHERE %s = $1", target.table, key), true
}

// existingColumns returns the "table.column" pairs present in the public schema,
// so the reset can address exactly what this database actually has.
func existingColumns(ctx context.Context, tx pgx.Tx) (map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT table_name, column_name FROM information_schema.columns WHERE table_schema = 'public'`)
	if err != nil {
		return nil, fmt.Errorf("inspect schema for demo wipe: %w", err)
	}
	defer rows.Close()

	present := map[string]bool{}
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			return nil, fmt.Errorf("inspect schema for demo wipe: %w", err)
		}
		present[table] = true
		present[table+"."+column] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inspect schema for demo wipe: %w", err)
	}
	return present, nil
}

// demoCompanyID resolves the tenant that owns the seeded demo user. A missing
// user is reported as 0 rather than as an error, which is how the reset stays a
// no-op on a database that was never seeded.
func demoCompanyID(ctx context.Context, tx pgx.Tx) (uint, error) {
	var companyID uint
	err := tx.QueryRow(ctx,
		`SELECT company_id FROM users WHERE LOWER(email) = $1 LIMIT 1`, demoEmail,
	).Scan(&companyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("resolve demo tenant: %w", err)
	}
	return companyID, nil
}

// NewPostgresStore establishes a connection pool to PostgreSQL.
func NewPostgresStore(ctx context.Context, connString string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("invalid postgres connection string: %w", err)
	}

	cfg.MaxConns = 15
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to postgres: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres ping failed: %w", err)
	}

	if err := MigrateTables(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migration failed: %w", err)
	}

	return &PostgresStore{pool: pool, db: pool}, nil
}

func (p *PostgresStore) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

func (p *PostgresStore) Ready(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// WithTransaction runs fn against a transactional view of the store.
//
// The Store interface predates context propagation - WithTransaction takes no
// context parameter - so this starts from context.Background() with a 30s budget
// instead of inheriting the request context. That is deliberate: a client that
// hangs up mid-posting must not be able to cancel a half-applied financial
// transaction halfway through, and the 30s ceiling is what bounds a posting
// that would otherwise hold the connection open. A future revision of the
// interface should take a context.Context and derive the deadline from it.
func (p *PostgresStore) WithTransaction(fn func(Store) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	transactional := &PostgresStore{pool: p.pool, db: tx}
	if err := fn(transactional); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- Auth & Users ---

func (p *PostgresStore) FindUserByEmail(email string) (*domain.User, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// LOWER() on both sides, not just the column: the caller may send
	// "User@Example.com", and idx_users_email_lower (migration 003) is built
	// on LOWER(email) so this predicate stays index-backed.
	query := `SELECT id, company_id, email, password, name, role FROM users WHERE LOWER(email) = LOWER($1) LIMIT 1`
	var u domain.User
	err := p.pool.QueryRow(ctx, query, strings.TrimSpace(email)).Scan(
		&u.ID, &u.CompanyID, &u.Email, &u.Password, &u.Name, &u.Role,
	)
	if err != nil {
		// The bool contract cannot express "the database is down"; a caller
		// that must not confuse the two uses the error-returning variants.
		return nil, false
	}
	return &u, true
}

// FindUsersByIDs resolves many users in one round trip, satisfying
// UserDirectory. An empty request is answered without touching the database.
func (p *PostgresStore) FindUsersByIDs(ids []uint) map[uint]*domain.User {
	found := map[uint]*domain.User{}
	if len(ids) == 0 {
		return found
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := p.pool.Query(ctx,
		`SELECT id, company_id, email, password, name, role FROM users WHERE id = ANY($1)`, ids)
	if err != nil {
		log.Printf("[Postgres] FindUsersByIDs: %v", err)
		return found
	}
	defer rows.Close()

	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.CompanyID, &u.Email, &u.Password, &u.Name, &u.Role); err != nil {
			log.Printf("[Postgres] FindUsersByIDs scan: %v", err)
			continue
		}
		copied := u
		found[u.ID] = &copied
	}
	if err := rows.Err(); err != nil {
		log.Printf("[Postgres] FindUsersByIDs rows: %v", err)
	}
	return found
}

func (p *PostgresStore) FindUserByID(id uint) (*domain.User, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, email, password, name, role FROM users WHERE id = $1 LIMIT 1`
	var u domain.User
	err := p.pool.QueryRow(ctx, query, id).Scan(
		&u.ID, &u.CompanyID, &u.Email, &u.Password, &u.Name, &u.Role,
	)
	if err != nil {
		return nil, false
	}
	return &u, true
}

func (p *PostgresStore) CreateUser(user *domain.User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	email := strings.ToLower(strings.TrimSpace(user.Email))
	query := `
		INSERT INTO users (company_id, email, password, name, role)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`
	err := p.pool.QueryRow(ctx, query, user.CompanyID, email, user.Password, user.Name, user.Role).Scan(&user.ID)
	if err != nil {
		if strings.Contains(err.Error(), "unique") || strings.Contains(err.Error(), "duplicate") {
			return domain.ErrConflict(domain.CodeDuplicate, "email is already registered")
		}
		return err
	}
	user.Email = email
	return nil
}

func (p *PostgresStore) UpdateUser(user *domain.User) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		UPDATE users
		SET name = $1, role = $2
		WHERE id = $3
	`
	tag, err := p.pool.Exec(ctx, query, user.Name, user.Role, user.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("user not found")
	}
	return nil
}

func (p *PostgresStore) CreateSession(session *domain.Session) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now()
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO sessions
		(token_hash, user_id, company_id, expires_at, created_at) VALUES ($1, $2, $3, $4, $5)`,
		session.TokenHash, session.UserID, session.CompanyID, session.ExpiresAt, session.CreatedAt)
	return err
}

func (p *PostgresStore) GetSession(tokenHash string, now time.Time) (*domain.Session, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var session domain.Session
	err := p.pool.QueryRow(ctx, `SELECT token_hash, user_id, company_id, expires_at, revoked_at, created_at
		FROM sessions WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2`, tokenHash, now).
		Scan(&session.TokenHash, &session.UserID, &session.CompanyID, &session.ExpiresAt, &session.RevokedAt, &session.CreatedAt)
	return &session, err == nil
}

func (p *PostgresStore) RevokeSession(tokenHash string, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $1 WHERE token_hash = $2 AND revoked_at IS NULL`, at, tokenHash)
	return err
}

func (p *PostgresStore) DeleteExpiredSessions(now time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= $1 OR revoked_at IS NOT NULL`, now)
	return err
}

// DeleteExpiredPasswordResetTokens drops spent and expired reset tokens. The
// auth layer sweeps on this same trigger as the session table, so neither grows
// without bound.
func (p *PostgresStore) DeleteExpiredPasswordResetTokens(ctx context.Context, now time.Time) error {
	_, err := p.db.Exec(ctx,
		`DELETE FROM password_reset_tokens WHERE used_at IS NOT NULL OR expires_at <= $1`, now)
	return err
}

// RevokeSessionsForUser ends every session belonging to a user.
//
// Called after a password reset: a bearer token captured before the reset would
// otherwise keep authenticating for the whole session TTL, which is exactly the
// window a stolen token is worth. Sessions are deleted rather than flagged so
// the table does not accumulate revoked rows.
func (p *PostgresStore) RevokeSessionsForUser(ctx context.Context, userID int64) error {
	if _, err := p.db.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("revoke sessions for user %d: %w", userID, err)
	}
	return nil
}

func (p *PostgresStore) CreatePasswordResetToken(token *domain.PasswordResetToken) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now()
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO password_reset_tokens
		(token_hash, user_id, expires_at, created_at) VALUES ($1, $2, $3, $4)`,
		token.TokenHash, token.UserID, token.ExpiresAt, token.CreatedAt)
	return err
}

// ResetPassword consumes a reset token and sets a new password, returning the
// user it acted on. The caller needs that id to revoke the account's live
// sessions: the token is the only link between the request and the account, and
// this call consumes it.
//
// The token row is locked FOR UPDATE, so two concurrent uses of the same token
// cannot both pass the used_at/expires_at check.
func (p *PostgresStore) ResetPassword(tokenHash, passwordHash string, now time.Time) (uint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var userID uint
	err = tx.QueryRow(ctx, `SELECT user_id FROM password_reset_tokens
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2 FOR UPDATE`, tokenHash, now).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, domain.ErrUnauthorized("password reset token is invalid or expired")
		}
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET password = $1 WHERE id = $2`, passwordHash, userID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE password_reset_tokens SET used_at = $1 WHERE token_hash = $2`, now, tokenHash); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return userID, nil
}

// --- Companies ---

func (p *PostgresStore) GetCompany(id uint) (*domain.Company, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, name, npwp, sak_mode, is_pkp, address, business_type FROM companies WHERE id = $1`
	var c domain.Company
	err := p.pool.QueryRow(ctx, query, id).Scan(
		&c.ID, &c.Name, &c.NPWP, &c.SAKMode, &c.IsPKP, &c.Address, &c.BusinessType,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("company not found")
		}
		return nil, err
	}
	return &c, nil
}

func (p *PostgresStore) CreateCompany(company *domain.Company) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		INSERT INTO companies (name, npwp, sak_mode, is_pkp, address, business_type)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`
	return p.pool.QueryRow(ctx, query,
		company.Name, company.NPWP, company.SAKMode, company.IsPKP, company.Address, company.BusinessType,
	).Scan(&company.ID)
}

func (p *PostgresStore) UpdateCompany(company *domain.Company) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		UPDATE companies
		SET name = $1, npwp = $2, sak_mode = $3, is_pkp = $4, address = $5, business_type = $6
		WHERE id = $7
	`
	tag, err := p.pool.Exec(ctx, query,
		company.Name, company.NPWP, company.SAKMode, company.IsPKP, company.Address, company.BusinessType, company.ID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("company not found")
	}
	return nil
}

// --- Branches ---

// ListBranches satisfies Store. It wraps ReadBranches, which is the only place
// the query lives; a failure is logged here because the slice contract cannot
// report it to the service layer.
func (p *PostgresStore) ListBranches(companyID uint) []domain.Branch {
	branches, err := p.ReadBranches(companyID)
	if err != nil {
		log.Printf("[Postgres] ListBranches(company=%d): %v", companyID, err)
		return []domain.Branch{}
	}
	return branches
}

// ReadBranches is the fallible form of ListBranches.
func (p *PostgresStore) ReadBranches(companyID uint) ([]domain.Branch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, name FROM branches WHERE company_id = $1 ORDER BY id ASC`
	rows, err := p.pool.Query(ctx, query, companyID)
	if err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	defer rows.Close()

	branches := []domain.Branch{}
	for rows.Next() {
		var b domain.Branch
		if err := rows.Scan(&b.ID, &b.CompanyID, &b.Name); err != nil {
			return nil, fmt.Errorf("list branches: %w", err)
		}
		branches = append(branches, b)
	}
	// A connection dropped mid-iteration reports here, not on Scan: without this
	// check a truncated result set is indistinguishable from a complete one.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list branches: %w", err)
	}
	return branches, nil
}

func (p *PostgresStore) GetBranch(companyID, id uint) (*domain.Branch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, name FROM branches WHERE company_id = $1 AND id = $2`
	var b domain.Branch
	err := p.pool.QueryRow(ctx, query, companyID, id).Scan(&b.ID, &b.CompanyID, &b.Name)
	if err != nil {
		// Only a genuinely absent row is a 404. Mapping every error to
		// ErrNotFound turned a dropped connection into "this branch does not
		// exist", which is a lie the caller cannot act on.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("branch not found")
		}
		return nil, fmt.Errorf("get branch %d: %w", id, err)
	}
	return &b, nil
}

func (p *PostgresStore) CreateBranch(branch *domain.Branch) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `INSERT INTO branches (company_id, name) VALUES ($1, $2) RETURNING id`
	return p.pool.QueryRow(ctx, query, branch.CompanyID, branch.Name).Scan(&branch.ID)
}

func (p *PostgresStore) UpdateBranch(branch *domain.Branch) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `UPDATE branches SET name = $1 WHERE company_id = $2 AND id = $3`
	tag, err := p.pool.Exec(ctx, query, branch.Name, branch.CompanyID, branch.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("branch not found")
	}
	return nil
}

func (p *PostgresStore) DeleteBranch(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `DELETE FROM branches WHERE company_id = $1 AND id = $2`
	tag, err := p.pool.Exec(ctx, query, companyID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("branch not found")
	}
	return nil
}

// --- Accounts ---

// ListAccounts satisfies Store; ReadAccounts holds the query and the error.
func (p *PostgresStore) ListAccounts(companyID uint) []domain.Account {
	accounts, err := p.ReadAccounts(companyID)
	if err != nil {
		log.Printf("[Postgres] ListAccounts(company=%d): %v", companyID, err)
		return []domain.Account{}
	}
	return accounts
}

// ReadAccounts is the fallible form of ListAccounts.
//
// The balance is derived here rather than read from accounts.balance: the
// column is a legacy cache (see migration 003) and can disagree with the
// posted journal lines it is supposed to summarise.
func (p *PostgresStore) ReadAccounts(companyID uint) ([]domain.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `SELECT a.id, a.company_id, a.code, a.name, a.type, a.parent_id, a.is_system, a.is_active,
		COALESCE(SUM(CASE WHEN j.status = $2 THEN
			CASE WHEN a.type IN ('asset', 'expense') THEN l.debit - l.credit ELSE l.credit - l.debit END
		ELSE 0 END), 0) AS balance
		FROM accounts a
		LEFT JOIN journal_lines l ON l.account_id = a.id
		LEFT JOIN journal_entries j ON j.id = l.journal_entry_id
		WHERE a.company_id = $1
		GROUP BY a.id ORDER BY a.code ASC`
	rows, err := p.pool.Query(ctx, query, companyID, domain.JournalStatusPosted)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	defer rows.Close()

	accounts := []domain.Account{}
	for rows.Next() {
		var a domain.Account
		if err := rows.Scan(&a.ID, &a.CompanyID, &a.Code, &a.Name, &a.Type, &a.ParentID, &a.IsSystem, &a.IsActive, &a.Balance); err != nil {
			return nil, fmt.Errorf("list accounts: %w", err)
		}
		accounts = append(accounts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	return accounts, nil
}

func (p *PostgresStore) GetAccount(companyID, id uint) (*domain.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT a.id, a.company_id, a.code, a.name, a.type, a.parent_id, a.is_system, a.is_active,
		COALESCE(SUM(CASE WHEN j.status = $3 THEN
			CASE WHEN a.type IN ('asset', 'expense') THEN l.debit - l.credit ELSE l.credit - l.debit END
		ELSE 0 END), 0) AS balance
		FROM accounts a
		LEFT JOIN journal_lines l ON l.account_id = a.id
		LEFT JOIN journal_entries j ON j.id = l.journal_entry_id
		WHERE a.company_id = $1 AND a.id = $2 GROUP BY a.id`
	var a domain.Account
	err := p.pool.QueryRow(ctx, query, companyID, id, domain.JournalStatusPosted).Scan(
		&a.ID, &a.CompanyID, &a.Code, &a.Name, &a.Type, &a.ParentID, &a.IsSystem, &a.IsActive, &a.Balance,
	)
	if err != nil {
		// A missing row is a 404; a transport or query failure has to surface as
		// an error, otherwise a database blip answers "no such account" and the
		// caller reports a phantom missing record to the user.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("account not found")
		}
		return nil, fmt.Errorf("get account %d: %w", id, err)
	}
	return &a, nil
}

func (p *PostgresStore) GetAccountByCode(companyID uint, code string) (*domain.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT a.id, a.company_id, a.code, a.name, a.type, a.parent_id, a.is_system, a.is_active,
		COALESCE(SUM(CASE WHEN j.status = $3 THEN
			CASE WHEN a.type IN ('asset', 'expense') THEN l.debit - l.credit ELSE l.credit - l.debit END
		ELSE 0 END), 0) AS balance
		FROM accounts a
		LEFT JOIN journal_lines l ON l.account_id = a.id
		LEFT JOIN journal_entries j ON j.id = l.journal_entry_id
		WHERE a.company_id = $1 AND a.code = $2 GROUP BY a.id`
	var a domain.Account
	err := p.pool.QueryRow(ctx, query, companyID, code, domain.JournalStatusPosted).Scan(
		&a.ID, &a.CompanyID, &a.Code, &a.Name, &a.Type, &a.ParentID, &a.IsSystem, &a.IsActive, &a.Balance,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("account not found")
		}
		return nil, fmt.Errorf("get account by code %q: %w", code, err)
	}
	return &a, nil
}

func (p *PostgresStore) CreateAccount(account *domain.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		INSERT INTO accounts (company_id, code, name, type, parent_id, is_system, is_active, balance)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0)
		RETURNING id
	`
	err := p.pool.QueryRow(ctx, query,
		account.CompanyID, account.Code, account.Name, account.Type, account.ParentID,
		account.IsSystem, account.IsActive,
	).Scan(&account.ID)
	if err != nil {
		if strings.Contains(err.Error(), "uq_company_account_code") || strings.Contains(err.Error(), "duplicate") {
			return domain.ErrConflict(domain.CodeDuplicate, "account code is already in use")
		}
		return err
	}
	return nil
}

func (p *PostgresStore) UpdateAccount(account *domain.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		UPDATE accounts
		SET name = $1, is_active = $2
		WHERE company_id = $3 AND id = $4
	`
	tag, err := p.pool.Exec(ctx, query, account.Name, account.IsActive, account.CompanyID, account.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("account not found")
	}
	return nil
}

func (p *PostgresStore) DeleteAccount(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check system and balance
	acc, err := p.GetAccount(companyID, id)
	if err != nil {
		return err
	}
	if acc.IsSystem {
		return domain.ErrForbidden("system accounts cannot be deleted")
	}
	if acc.Balance != 0 {
		return domain.ErrValidation("an account with a balance cannot be deleted")
	}

	query := `DELETE FROM accounts WHERE company_id = $1 AND id = $2`
	tag, err := p.pool.Exec(ctx, query, companyID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("account not found")
	}
	return nil
}

// --- Journals ---

// ListJournalEntries satisfies Store; ReadJournalEntries holds the query.
func (p *PostgresStore) ListJournalEntries(companyID uint, status string) []domain.JournalEntry {
	entries, err := p.ReadJournalEntries(companyID, status)
	if err != nil {
		log.Printf("[Postgres] ListJournalEntries(company=%d, status=%q): %v", companyID, status, err)
		return []domain.JournalEntry{}
	}
	return entries
}

// ReadJournalEntries is the fallible form of ListJournalEntries.
//
// Cost: 1 + N queries, one for the headers and one per entry for its lines.
// That N+1 is deliberate and unchanged: the caller (the list handler) filters,
// sorts and paginates the whole tenant in memory, so it already pays for every
// row, and collapsing the lines into one IN (...) query would change row
// ordering guarantees that the ledger reports depend on. idx_journal_lines_entry
// (migration 001) keeps the per-entry lookup an index scan. Revisit with a
// keyset-paginated query if a tenant ever outgrows a single in-memory page.
func (p *PostgresStore) ReadJournalEntries(companyID uint, status string) ([]domain.JournalEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var query string
	var args []any
	if status != "" {
		query = `SELECT id, company_id, branch_id, number, date, status, source_type, source_id, memo, created_at
				 FROM journal_entries WHERE company_id = $1 AND status = $2 ORDER BY number DESC`
		args = []any{companyID, status}
	} else {
		query = `SELECT id, company_id, branch_id, number, date, status, source_type, source_id, memo, created_at
				 FROM journal_entries WHERE company_id = $1 ORDER BY number DESC`
		args = []any{companyID}
	}

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list journal entries: %w", err)
	}
	defer rows.Close()

	entries := []domain.JournalEntry{}
	for rows.Next() {
		var e domain.JournalEntry
		if err := rows.Scan(
			&e.ID, &e.CompanyID, &e.BranchID, &e.Number, &e.Date, &e.Status,
			&e.SourceType, &e.SourceID, &e.Memo, &e.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list journal entries: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list journal entries: %w", err)
	}
	rows.Close()

	for i := range entries {
		lines, err := p.readJournalLines(ctx, entries[i].ID)
		if err != nil {
			return nil, err
		}
		entries[i].Lines = lines
	}
	return entries, nil
}

// readJournalLines loads one entry's lines in insertion order.
func (p *PostgresStore) readJournalLines(ctx context.Context, entryID uint) ([]domain.JournalLine, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, journal_entry_id, account_id, debit, credit, memo
		 FROM journal_lines WHERE journal_entry_id = $1 ORDER BY id ASC`, entryID)
	if err != nil {
		return nil, fmt.Errorf("list journal lines for entry %d: %w", entryID, err)
	}
	defer rows.Close()

	lines := []domain.JournalLine{}
	for rows.Next() {
		var l domain.JournalLine
		if err := rows.Scan(&l.ID, &l.JournalEntryID, &l.AccountID, &l.Debit, &l.Credit, &l.Memo); err != nil {
			return nil, fmt.Errorf("list journal lines for entry %d: %w", entryID, err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list journal lines for entry %d: %w", entryID, err)
	}
	return lines, nil
}

func (p *PostgresStore) GetJournalEntry(companyID, id uint) (*domain.JournalEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, branch_id, number, date, status, source_type, source_id, memo, created_at
			  FROM journal_entries WHERE company_id = $1 AND id = $2`
	var e domain.JournalEntry
	err := p.pool.QueryRow(ctx, query, companyID, id).Scan(
		&e.ID, &e.CompanyID, &e.BranchID, &e.Number, &e.Date, &e.Status,
		&e.SourceType, &e.SourceID, &e.Memo, &e.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("journal entry not found")
		}
		return nil, fmt.Errorf("get journal entry %d: %w", id, err)
	}

	// A failure to load the lines used to leave the entry with no lines at all,
	// which reads as "a journal with no postings" - a balanced-entry check on
	// that would then pass for the wrong reason.
	lines, err := p.readJournalLines(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	e.Lines = lines
	return &e, nil
}

func (p *PostgresStore) CreateJournalEntry(entry *domain.JournalEntry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	query := `
		INSERT INTO journal_entries (company_id, branch_id, number, date, status, source_type, source_id, memo, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	err = tx.QueryRow(ctx, query,
		entry.CompanyID, entry.BranchID, entry.Number, entry.Date, entry.Status,
		entry.SourceType, entry.SourceID, entry.Memo, entry.CreatedAt,
	).Scan(&entry.ID)
	if err != nil {
		return err
	}

	lineQuery := `
		INSERT INTO journal_lines (journal_entry_id, account_id, debit, credit, memo)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`
	for i := range entry.Lines {
		entry.Lines[i].JournalEntryID = entry.ID
		err = tx.QueryRow(ctx, lineQuery,
			entry.ID, entry.Lines[i].AccountID, entry.Lines[i].Debit, entry.Lines[i].Credit, entry.Lines[i].Memo,
		).Scan(&entry.Lines[i].ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (p *PostgresStore) UpdateJournalEntry(entry *domain.JournalEntry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE journal_entries
		SET branch_id = $1, date = $2, source_type = $3, source_id = $4, memo = $5
		WHERE company_id = $6 AND id = $7 AND status = $8`,
		entry.BranchID, entry.Date, entry.SourceType, entry.SourceID, entry.Memo,
		entry.CompanyID, entry.ID, domain.JournalStatusDraft)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConflict(domain.CodeImmutable, "only draft journals can be edited")
	}
	if _, err = tx.Exec(ctx, `DELETE FROM journal_lines WHERE journal_entry_id = $1`, entry.ID); err != nil {
		return err
	}
	for i := range entry.Lines {
		entry.Lines[i].JournalEntryID = entry.ID
		if err = tx.QueryRow(ctx, `INSERT INTO journal_lines (journal_entry_id, account_id, debit, credit, memo)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`, entry.ID, entry.Lines[i].AccountID,
			entry.Lines[i].Debit, entry.Lines[i].Credit, entry.Lines[i].Memo).Scan(&entry.Lines[i].ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *PostgresStore) PostJournalEntry(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := p.db.Exec(ctx, `UPDATE journal_entries SET status = $1
		WHERE company_id = $2 AND id = $3 AND status = $4`,
		domain.JournalStatusPosted, companyID, id, domain.JournalStatusDraft)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConflict(domain.CodeImmutable, "journal entry was not found or has already been posted")
	}
	return nil
}

func (p *PostgresStore) DeleteJournalEntry(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := p.db.Exec(ctx, `DELETE FROM journal_entries
		WHERE company_id = $1 AND id = $2 AND status = $3`, companyID, id, domain.JournalStatusDraft)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConflict(domain.CodeImmutable, "only draft journals can be deleted")
	}
	return nil
}

// ListLedgerEntries satisfies Store; ReadLedgerEntries holds the query.
func (p *PostgresStore) ListLedgerEntries(companyID, accountID uint) []domain.LedgerEntry {
	entries, err := p.ReadLedgerEntries(companyID, accountID)
	if err != nil {
		log.Printf("[Postgres] ListLedgerEntries(company=%d, account=%d): %v", companyID, accountID, err)
		return []domain.LedgerEntry{}
	}
	return entries
}

// ReadLedgerEntries is the fallible form of ListLedgerEntries. It is driven by
// idx_journal_lines_account (migration 003), the index that was missing before
// and made every ledger view scan the whole line table.
func (p *PostgresStore) ReadLedgerEntries(companyID, accountID uint) ([]domain.LedgerEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `
		SELECT e.date, e.number, COALESCE(NULLIF(l.memo, ''), e.memo), l.debit, l.credit
		FROM journal_lines l
		JOIN journal_entries e ON l.journal_entry_id = e.id
		WHERE e.company_id = $1 AND l.account_id = $2 AND e.status = $3
		ORDER BY e.date ASC, e.id ASC
	`
	rows, err := p.pool.Query(ctx, query, companyID, accountID, domain.JournalStatusPosted)
	if err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	defer rows.Close()

	entries := []domain.LedgerEntry{}
	for rows.Next() {
		var le domain.LedgerEntry
		if err := rows.Scan(&le.Date, &le.Ref, &le.Memo, &le.Debit, &le.Credit); err != nil {
			return nil, fmt.Errorf("list ledger entries: %w", err)
		}
		entries = append(entries, le)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ledger entries: %w", err)
	}
	return entries, nil
}

func (p *PostgresStore) NextNumber(companyID uint, prefix string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		INSERT INTO number_sequences (company_id, prefix, last_number)
		VALUES ($1, $2, 1)
		ON CONFLICT (company_id, prefix)
		DO UPDATE SET last_number = number_sequences.last_number + 1
		RETURNING last_number
	`
	var nextNum int
	err := p.db.QueryRow(ctx, query, companyID, prefix).Scan(&nextNum)
	if err != nil {
		return fmt.Sprintf("%s-%04d", prefix, time.Now().Unix()%10000)
	}
	return fmt.Sprintf("%s-%04d", prefix, nextNum)
}

// SetNextNumber pins a sequence so the next generated number is value+1.
// Used by the seeder to continue after seeded document numbers.
func (p *PostgresStore) SetNextNumber(companyID uint, prefix string, value int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		INSERT INTO number_sequences (company_id, prefix, last_number)
		VALUES ($1, $2, $3)
		ON CONFLICT (company_id, prefix)
		DO UPDATE SET last_number = GREATEST(number_sequences.last_number, EXCLUDED.last_number)
	`
	if _, err := p.pool.Exec(ctx, query, companyID, prefix, value); err != nil {
		log.Printf("[Seed] could not pin %s sequence for company %d: %v", prefix, companyID, err)
	}
}

// --- Invoices ---

// ListInvoices satisfies Store; ReadInvoices holds the query.
func (p *PostgresStore) ListInvoices(companyID uint) []domain.Invoice {
	invoices, err := p.ReadInvoices(companyID)
	if err != nil {
		log.Printf("[Postgres] ListInvoices(company=%d): %v", companyID, err)
		return []domain.Invoice{}
	}
	return invoices
}

// ReadInvoices is the fallible form of ListInvoices.
//
// Cost: 1 + N queries, one header query plus one per invoice for its lines.
// Kept as-is for the same reason as ReadJournalEntries: the caller filters,
// sorts and paginates the whole tenant in memory, and idx_invoice_lines_invoice
// keeps each per-invoice lookup an index scan. A batched IN (...) fetch would
// need its own ordering guarantee to keep line order stable per document.
func (p *PostgresStore) ReadInvoices(companyID uint) ([]domain.Invoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	query := `SELECT id, company_id, branch_id, number, customer_id, customer_name, date, due_date, status, notes, subtotal, tax_amount, total, created_at
			  FROM invoices WHERE company_id = $1 ORDER BY created_at DESC, number DESC`
	rows, err := p.pool.Query(ctx, query, companyID)
	if err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	defer rows.Close()

	invoices := []domain.Invoice{}
	for rows.Next() {
		var inv domain.Invoice
		if err := rows.Scan(
			&inv.ID, &inv.CompanyID, &inv.BranchID, &inv.Number, &inv.CustomerID, &inv.CustomerName,
			&inv.Date, &inv.DueDate, &inv.Status, &inv.Notes, &inv.Subtotal, &inv.TaxAmount, &inv.Total, &inv.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("list invoices: %w", err)
		}
		invoices = append(invoices, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invoices: %w", err)
	}
	rows.Close()

	for i := range invoices {
		lines, err := p.readInvoiceLines(ctx, invoices[i].ID)
		if err != nil {
			return nil, err
		}
		invoices[i].Lines = lines
	}
	return invoices, nil
}

// readInvoiceLines loads one invoice's lines in insertion order.
func (p *PostgresStore) readInvoiceLines(ctx context.Context, invoiceID uint) ([]domain.InvoiceLine, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, invoice_id, name, qty, price, discount_percent, subtotal
		 FROM invoice_lines WHERE invoice_id = $1 ORDER BY id ASC`, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list invoice lines for invoice %d: %w", invoiceID, err)
	}
	defer rows.Close()

	lines := []domain.InvoiceLine{}
	for rows.Next() {
		var l domain.InvoiceLine
		if err := rows.Scan(&l.ID, &l.InvoiceID, &l.Name, &l.Qty, &l.Price, &l.DiscountPercent, &l.Subtotal); err != nil {
			return nil, fmt.Errorf("list invoice lines for invoice %d: %w", invoiceID, err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invoice lines for invoice %d: %w", invoiceID, err)
	}
	return lines, nil
}

func (p *PostgresStore) GetInvoice(companyID, id uint) (*domain.Invoice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, branch_id, number, customer_id, customer_name, date, due_date, status, notes, subtotal, tax_amount, total, created_at
			  FROM invoices WHERE company_id = $1 AND id = $2`
	var inv domain.Invoice
	err := p.pool.QueryRow(ctx, query, companyID, id).Scan(
		&inv.ID, &inv.CompanyID, &inv.BranchID, &inv.Number, &inv.CustomerID, &inv.CustomerName,
		&inv.Date, &inv.DueDate, &inv.Status, &inv.Notes, &inv.Subtotal, &inv.TaxAmount, &inv.Total, &inv.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("invoice not found")
		}
		return nil, fmt.Errorf("get invoice %d: %w", id, err)
	}

	// Missing lines used to yield an invoice whose total no longer matches its
	// own lines, which is exactly the state the issue flow must refuse.
	lines, err := p.readInvoiceLines(ctx, inv.ID)
	if err != nil {
		return nil, err
	}
	inv.Lines = lines
	return &inv, nil
}

func (p *PostgresStore) CreateInvoice(invoice *domain.Invoice) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	query := `
		INSERT INTO invoices (company_id, branch_id, number, customer_id, customer_name, date, due_date, status, notes, subtotal, tax_amount, total, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING id
	`
	if invoice.CreatedAt.IsZero() {
		invoice.CreatedAt = time.Now()
	}
	err = tx.QueryRow(ctx, query,
		invoice.CompanyID, invoice.BranchID, invoice.Number, invoice.CustomerID, invoice.CustomerName,
		invoice.Date, invoice.DueDate, invoice.Status, invoice.Notes, invoice.Subtotal, invoice.TaxAmount, invoice.Total, invoice.CreatedAt,
	).Scan(&invoice.ID)
	if err != nil {
		return err
	}

	lineQuery := `
		INSERT INTO invoice_lines (invoice_id, name, qty, price, discount_percent, subtotal)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`
	for i := range invoice.Lines {
		invoice.Lines[i].InvoiceID = invoice.ID
		err = tx.QueryRow(ctx, lineQuery,
			invoice.ID, invoice.Lines[i].Name, invoice.Lines[i].Qty, invoice.Lines[i].Price,
			invoice.Lines[i].DiscountPercent, invoice.Lines[i].Subtotal,
		).Scan(&invoice.Lines[i].ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (p *PostgresStore) UpdateInvoice(invoice *domain.Invoice) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := p.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	query := `UPDATE invoices SET branch_id = $1, customer_id = $2, customer_name = $3,
		date = $4, due_date = $5, status = $6, notes = $7, subtotal = $8,
		tax_amount = $9, total = $10 WHERE company_id = $11 AND id = $12 AND status = 'Draft'`
	tag, err := tx.Exec(ctx, query, invoice.BranchID, invoice.CustomerID, invoice.CustomerName,
		invoice.Date, invoice.DueDate, invoice.Status, invoice.Notes, invoice.Subtotal,
		invoice.TaxAmount, invoice.Total, invoice.CompanyID, invoice.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConflict(domain.CodeImmutable, "only draft invoices can be edited")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1`, invoice.ID); err != nil {
		return err
	}
	const lineQuery = `INSERT INTO invoice_lines (invoice_id, name, qty, price, discount_percent, subtotal)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`
	for i := range invoice.Lines {
		invoice.Lines[i].InvoiceID = invoice.ID
		if err := tx.QueryRow(ctx, lineQuery, invoice.ID, invoice.Lines[i].Name, invoice.Lines[i].Qty,
			invoice.Lines[i].Price, invoice.Lines[i].DiscountPercent, invoice.Lines[i].Subtotal).Scan(&invoice.Lines[i].ID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *PostgresStore) TransitionInvoiceStatus(companyID, id uint, from, to string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tag, err := p.db.Exec(ctx, `UPDATE invoices SET status = $1 WHERE company_id = $2 AND id = $3 AND status = $4`, to, companyID, id, from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrConflict(domain.CodeImmutable, "invoice status has changed")
	}
	return nil
}

func (p *PostgresStore) DeleteInvoice(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `DELETE FROM invoices WHERE company_id = $1 AND id = $2`
	tag, err := p.db.Exec(ctx, query, companyID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("invoice not found")
	}
	return nil
}

// ListInvoicePayments satisfies Store; ReadInvoicePayments holds the query.
func (p *PostgresStore) ListInvoicePayments(companyID, invoiceID uint) []domain.InvoicePayment {
	payments, err := p.ReadInvoicePayments(companyID, invoiceID)
	if err != nil {
		log.Printf("[Postgres] ListInvoicePayments(company=%d, invoice=%d): %v", companyID, invoiceID, err)
		return []domain.InvoicePayment{}
	}
	return payments
}

// ReadInvoicePayments is the fallible form of ListInvoicePayments.
//
// This one is load-bearing beyond the list view: the service derives an
// invoice's paid and outstanding amounts from it, so a swallowed error here
// reported every invoice as fully unpaid.
func (p *PostgresStore) ReadInvoicePayments(companyID, invoiceID uint) ([]domain.InvoicePayment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := p.pool.Query(ctx, `SELECT id, company_id, invoice_id, cash_account_id, date, amount, notes, journal_id, created_at
		FROM invoice_payments WHERE company_id = $1 AND invoice_id = $2 ORDER BY date, id`, companyID, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("list invoice payments: %w", err)
	}
	defer rows.Close()
	out := make([]domain.InvoicePayment, 0)
	for rows.Next() {
		var payment domain.InvoicePayment
		if err := rows.Scan(&payment.ID, &payment.CompanyID, &payment.InvoiceID, &payment.CashAccountID,
			&payment.Date, &payment.Amount, &payment.Notes, &payment.JournalID, &payment.CreatedAt); err != nil {
			return nil, fmt.Errorf("list invoice payments: %w", err)
		}
		out = append(out, payment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list invoice payments: %w", err)
	}
	return out, nil
}

func (p *PostgresStore) CreateInvoicePayment(payment *domain.InvoicePayment) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if payment.CreatedAt.IsZero() {
		payment.CreatedAt = time.Now()
	}
	return p.db.QueryRow(ctx, `INSERT INTO invoice_payments
		(company_id, invoice_id, cash_account_id, date, amount, notes, journal_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`, payment.CompanyID,
		payment.InvoiceID, payment.CashAccountID, payment.Date, payment.Amount, payment.Notes,
		payment.JournalID, payment.CreatedAt).Scan(&payment.ID)
}

// --- Customers ---

// ListCustomers satisfies Store; ReadCustomers holds the query.
func (p *PostgresStore) ListCustomers(companyID uint) []domain.Customer {
	customers, err := p.ReadCustomers(companyID)
	if err != nil {
		log.Printf("[Postgres] ListCustomers(company=%d): %v", companyID, err)
		return []domain.Customer{}
	}
	return customers
}

// ReadCustomers is the fallible form of ListCustomers.
func (p *PostgresStore) ReadCustomers(companyID uint) ([]domain.Customer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, name, email, phone, address FROM customers WHERE company_id = $1 ORDER BY name ASC`
	rows, err := p.pool.Query(ctx, query, companyID)
	if err != nil {
		return nil, fmt.Errorf("list customers: %w", err)
	}
	defer rows.Close()

	customers := []domain.Customer{}
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.CompanyID, &c.Name, &c.Email, &c.Phone, &c.Address); err != nil {
			return nil, fmt.Errorf("list customers: %w", err)
		}
		customers = append(customers, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list customers: %w", err)
	}
	return customers, nil
}

func (p *PostgresStore) GetCustomer(companyID, id uint) (*domain.Customer, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, name, email, phone, address FROM customers WHERE company_id = $1 AND id = $2`
	var c domain.Customer
	err := p.pool.QueryRow(ctx, query, companyID, id).Scan(&c.ID, &c.CompanyID, &c.Name, &c.Email, &c.Phone, &c.Address)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound("customer not found")
		}
		return nil, fmt.Errorf("get customer %d: %w", id, err)
	}
	return &c, nil
}

func (p *PostgresStore) CreateCustomer(customer *domain.Customer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `INSERT INTO customers (company_id, name, email, phone, address) VALUES ($1, $2, $3, $4, $5) RETURNING id`
	return p.db.QueryRow(ctx, query, customer.CompanyID, customer.Name, customer.Email, customer.Phone, customer.Address).Scan(&customer.ID)
}

func (p *PostgresStore) UpdateCustomer(customer *domain.Customer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `UPDATE customers SET name = $1, email = $2, phone = $3, address = $4 WHERE company_id = $5 AND id = $6`
	tag, err := p.pool.Exec(ctx, query, customer.Name, customer.Email, customer.Phone, customer.Address, customer.CompanyID, customer.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("customer not found")
	}
	return nil
}

func (p *PostgresStore) DeleteCustomer(companyID, id uint) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `DELETE FROM customers WHERE company_id = $1 AND id = $2`
	tag, err := p.pool.Exec(ctx, query, companyID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound("customer not found")
	}
	return nil
}

// --- Audit ---

func (p *PostgresStore) AppendAuditLog(logEntry domain.AuditLog) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `
		INSERT INTO audit_logs (company_id, user_id, action, entity, before_json, after_json, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
	if logEntry.CreatedAt.IsZero() {
		logEntry.CreatedAt = time.Now()
	}
	_, err := p.db.Exec(ctx, query,
		logEntry.CompanyID, logEntry.UserID, logEntry.Action, logEntry.Entity,
		logEntry.BeforeJSON, logEntry.AfterJSON, logEntry.CreatedAt,
	)
	return err
}

// ListAuditLogs satisfies Store; ReadAuditLogs holds the query and the ordering.
func (p *PostgresStore) ListAuditLogs(companyID uint) []domain.AuditLog {
	logs, err := p.ReadAuditLogs(companyID)
	if err != nil {
		log.Printf("[Postgres] ListAuditLogs(company=%d): %v", companyID, err)
		return []domain.AuditLog{}
	}
	return logs
}

// ReadAuditLogs is the fallible form of ListAuditLogs.
//
// Newest first by event time. Ordering by id alone would surface the seeded
// trail in the order rows were inserted, which is grouped by document status
// rather than by date; id is the tie-breaker so events sharing a timestamp
// (seeded entries all land at 09:00) stay in a stable order.
// idx_audit_logs_company_created (migration 003) matches this ORDER BY exactly.
func (p *PostgresStore) ReadAuditLogs(companyID uint) ([]domain.AuditLog, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := `SELECT id, company_id, user_id, action, entity, before_json, after_json, created_at
			  FROM audit_logs WHERE company_id = $1 ORDER BY created_at DESC, id DESC`
	rows, err := p.pool.Query(ctx, query, companyID)
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	defer rows.Close()

	logs := []domain.AuditLog{}
	for rows.Next() {
		var al domain.AuditLog
		if err := rows.Scan(&al.ID, &al.CompanyID, &al.UserID, &al.Action, &al.Entity, &al.BeforeJSON, &al.AfterJSON, &al.CreatedAt); err != nil {
			return nil, fmt.Errorf("list audit logs: %w", err)
		}
		logs = append(logs, al)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	return logs, nil
}
