package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"tera/internal/domain"
	"tera/internal/security"
)

// MemoryStore is a mutex-guarded in-memory implementation of Store.
// It is seeded with a single demo company whose balances mirror the
// frontend's mock data so the UI renders consistent values.
type MemoryStore struct {
	mu sync.RWMutex
	// inTx is set while WithTransaction runs its closure. The lock helpers below
	// check it, so a closure that reaches back into the parent store fails
	// immediately with a named cause instead of hanging on a non-reentrant
	// mutex.
	inTx        atomic.Bool
	users       map[string]*domain.User // key: email
	companies   map[uint]*domain.Company
	branches    []domain.Branch
	accounts    []domain.Account
	journals    []domain.JournalEntry
	invoices    []domain.Invoice
	payments    []domain.InvoicePayment
	customers   []domain.Customer
	auditLogs   []domain.AuditLog
	sessions    map[string]*domain.Session
	resetTokens map[string]*domain.PasswordResetToken

	idCounter uint
	numbers   map[string]int // key: "companyID-prefix" -> last sequence number
}

// NewMemoryStore seeds a demo company and returns a ready store.
func NewMemoryStore() *MemoryStore {
	s := &MemoryStore{
		users:       map[string]*domain.User{},
		companies:   map[uint]*domain.Company{},
		numbers:     map[string]int{},
		sessions:    map[string]*domain.Session{},
		resetTokens: map[string]*domain.PasswordResetToken{},
	}
	s.seed()
	return s
}

func (s *MemoryStore) Ready(context.Context) error { return nil }

// lock and rLock are the only supported way to reach the mutex.
//
// sync.RWMutex is not reentrant and WithTransaction holds the write lock for the
// whole closure, so a closure that calls back into the store it was opened on
// would block forever. Every accessor goes through here, so that mistake
// surfaces as a panic naming the cause instead of a hung request - a
// transaction must only ever touch the Store handed to its closure, which is a
// private copy whose writes are discarded unless the closure returns nil.
func (s *MemoryStore) lock() {
	s.guardReentrancy()
	s.mu.Lock()
}

func (s *MemoryStore) rLock() {
	s.guardReentrancy()
	s.mu.RLock()
}

func (s *MemoryStore) guardReentrancy() {
	if s.inTx.Load() {
		panic("repository.MemoryStore: re-entrant access from inside WithTransaction; " +
			"use the Store argument passed to the closure instead of the store it was opened on")
	}
}

func (s *MemoryStore) WithTransaction(fn func(Store) error) error {
	s.lock()
	// Set after the lock is held and cleared before it is released (defers run
	// last-in-first-out), so no caller can observe a store that is neither
	// locked nor marked.
	s.inTx.Store(true)
	defer s.inTx.Store(false)
	defer s.mu.Unlock()
	working := s.cloneLocked()
	if err := fn(working); err != nil {
		return err
	}
	s.users = working.users
	s.companies = working.companies
	s.branches = working.branches
	s.accounts = working.accounts
	s.journals = working.journals
	s.invoices = working.invoices
	s.payments = working.payments
	s.customers = working.customers
	s.auditLogs = working.auditLogs
	s.sessions = working.sessions
	s.resetTokens = working.resetTokens
	s.idCounter = working.idCounter
	s.numbers = working.numbers
	return nil
}

func (s *MemoryStore) cloneLocked() *MemoryStore {
	clone := &MemoryStore{
		users: map[string]*domain.User{}, companies: map[uint]*domain.Company{},
		sessions: map[string]*domain.Session{}, resetTokens: map[string]*domain.PasswordResetToken{},
		numbers: map[string]int{}, idCounter: s.idCounter,
	}
	for key, value := range s.users {
		copy := *value
		clone.users[key] = &copy
	}
	for key, value := range s.companies {
		copy := *value
		clone.companies[key] = &copy
	}
	for key, value := range s.sessions {
		copy := *value
		clone.sessions[key] = &copy
	}
	for key, value := range s.resetTokens {
		copy := *value
		clone.resetTokens[key] = &copy
	}
	for key, value := range s.numbers {
		clone.numbers[key] = value
	}
	clone.branches = append([]domain.Branch(nil), s.branches...)
	clone.accounts = append([]domain.Account(nil), s.accounts...)
	clone.customers = append([]domain.Customer(nil), s.customers...)
	clone.payments = append([]domain.InvoicePayment(nil), s.payments...)
	clone.auditLogs = append([]domain.AuditLog(nil), s.auditLogs...)
	clone.journals = make([]domain.JournalEntry, len(s.journals))
	for i, entry := range s.journals {
		clone.journals[i] = entry
		clone.journals[i].Lines = append([]domain.JournalLine(nil), entry.Lines...)
	}
	clone.invoices = make([]domain.Invoice, len(s.invoices))
	for i, invoice := range s.invoices {
		clone.invoices[i] = invoice
		clone.invoices[i].Lines = append([]domain.InvoiceLine(nil), invoice.Lines...)
	}
	return clone
}

func (s *MemoryStore) nextID() uint {
	s.idCounter++
	return s.idCounter
}

func (s *MemoryStore) seed() {
	company := &domain.Company{
		ID:           s.nextID(),
		Name:         "PT Tera Sejahtera",
		NPWP:         "01.234.567.8-901.000",
		SAKMode:      domain.SAKEMKM,
		IsPKP:        true,
		Address:      "Jl. Ledger No. 10, Bandung",
		BusinessType: "Trading & Services",
	}
	s.companies[company.ID] = company

	s.branches = []domain.Branch{
		{ID: s.nextID(), CompanyID: company.ID, Name: "Kantor Pusat — Bandung"},
		{ID: s.nextID(), CompanyID: company.ID, Name: "Cabang Cimahi"},
		{ID: s.nextID(), CompanyID: company.ID, Name: "Gudang Surabaya"},
	}
	branchIDs := []uint{s.branches[0].ID, s.branches[1].ID, s.branches[2].ID}

	hashed, err := security.HashPassword(demoPassword)
	if err != nil {
		// Unrecoverable by design: the demo tenant ships with exactly one
		// credential, so a store that cannot hash it has no usable state and
		// must not be handed to a caller.
		panic("repository.NewMemoryStore: could not hash the built-in demo password, " +
			"so the seeded demo user cannot authenticate: " + err.Error())
	}
	s.users[demoEmail] = &domain.User{
		ID:        s.nextID(),
		CompanyID: company.ID,
		Email:     demoEmail,
		Password:  hashed,
		Name:      "Demo User",
		Role:      "Owner",
	}
	// --- Chart of Accounts (hierarchical; ParentID links header groups) ---
	// Parent rows are created before their children because demoCOA lists
	// groups first, so a single pass resolves every parent reference.
	idByCode := map[string]uint{}
	for _, row := range demoCOA() {
		account := domain.Account{
			ID:        s.nextID(),
			CompanyID: company.ID,
			Code:      row.code,
			Name:      row.name,
			Type:      row.typ,
			ParentID:  idByCode[row.parent],
			IsSystem:  row.isSystem,
			IsActive:  true,
			Balance:   0,
		}
		s.accounts = append(s.accounts, account)
		idByCode[row.code] = account.ID
	}
	opening := domain.JournalEntry{
		ID: s.nextID(), CompanyID: company.ID, BranchID: branchIDs[0], Number: "OPEN-0001",
		Date: "2000-01-01", Status: domain.JournalStatusPosted, SourceType: domain.SourceManual,
		Memo: "Opening balances", CreatedAt: time.Now(), Lines: demoOpeningLines(idByCode),
	}
	for i := range opening.Lines {
		opening.Lines[i].ID = s.nextID()
		opening.Lines[i].JournalEntryID = opening.ID
	}
	s.journals = append(s.journals, opening)

	// --- Customers ---
	for _, customer := range demoCustomers() {
		copy := customer
		copy.ID = s.nextID()
		copy.CompanyID = company.ID
		s.customers = append(s.customers, copy)
	}

	// --- Invoices ---
	for _, seed := range buildDemoInvoices() {
		invoice := s.materialiseInvoice(
			company.ID,
			branchIDs[seed.branchIndex],
			seed.number,
			seed.customer,
			seed.issueDate(),
			seed.dueDate(),
			seed.status,
			seed.subtotal,
			seed.taxAmount,
		)
		s.invoices = append(s.invoices, invoice)

		// Mirror InvoiceService.Create so the in-memory quick start shows the
		// same audit trail as the PostgreSQL path, rather than a single
		// placeholder row.
		after, _ := json.Marshal(map[string]any{
			"number": invoice.Number,
			"total":  invoice.Total,
		})
		s.appendAuditAt(company.ID, s.users[demoEmail].ID, "invoice.create", "invoice",
			string(after), issueTime(invoice.Date))
	}
	s.numbers[sequenceKey(company.ID, "INV")] = 241

	// --- Journal entries ---
	// Each seeded entry is posted through applySeedEntry, which also rolls the
	// account balances forward. This is what keeps the balance sheet balanced:
	// balances are derived from the journals rather than asserted alongside
	// them.
	highestJournal := 0
	// Mirrors SeedDemoData: the generated monthly history is seeded alongside the
	// current-period entries so the in-memory quick start and the PostgreSQL
	// demo tenant present the identical six-month ledger. History first keeps
	// the current-period numbers at the top of the descending sort.
	for _, seed := range append(demoHistoryJournals(), demoJournals()...) {
		entry := domain.JournalEntry{
			ID: s.nextID(), CompanyID: company.ID, BranchID: branchIDs[0],
			Number: seed.number, Date: seed.entryDate(), Status: seed.status,
			SourceType: seed.source, Memo: seed.memo, CreatedAt: time.Now(),
		}
		for _, lineSeed := range seed.lines {
			entry.Lines = append(entry.Lines, domain.JournalLine{
				ID: s.nextID(), JournalEntryID: entry.ID,
				AccountID: idByCode[lineSeed.accountCode],
				Debit:     lineSeed.debit, Credit: lineSeed.credit, Memo: lineSeed.memo,
			})
		}
		s.journals = append(s.journals, entry)

		var totalDebit int64
		for _, line := range entry.Lines {
			totalDebit += line.Debit
		}
		journalAfter, _ := json.Marshal(map[string]any{
			"number": entry.Number,
			"memo":   entry.Memo,
			"total":  totalDebit,
		})
		action := "journal.create"
		if entry.Status == domain.JournalStatusPosted {
			action = "journal.post"
		}
		s.appendAuditAt(company.ID, s.users[demoEmail].ID, action, "journal_entry",
			string(journalAfter), issueTime(entry.Date))

		highestJournal = max(highestJournal, numericSuffix(seed.number))
	}
	s.numbers[sequenceKey(company.ID, "JRN")] = highestJournal
}

// balanceDeltaFor returns the signed change to a balance given the account's
// normal side. Asset and expense accounts are debit-normal; liability, equity
// and revenue accounts are credit-normal.
func balanceDeltaFor(accountType string, debit, credit int64) int64 {
	switch accountType {
	case domain.TypeAsset, domain.TypeExpense:
		return debit - credit
	default:
		return credit - debit
	}
}

// numericSuffix extracts the trailing integer from a document number such as
// "JRN-1063" so the next generated number continues the sequence.
func numericSuffix(value string) int {
	digits := 0
	seenDigit := false
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] < '0' || value[i] > '9' {
			break
		}
		digits++
		seenDigit = true
	}
	if !seenDigit {
		return 0
	}
	n := 0
	for _, char := range value[len(value)-digits:] {
		n = n*10 + int(char-'0')
	}
	return n
}

// materialiseInvoice converts a declarative seed row into a stored invoice,
// resolving the customer ID and building a balanced line item.
func (s *MemoryStore) materialiseInvoice(
	companyID, branchID uint,
	number, customer, date, due, status string,
	subtotal, taxAmount int64,
) domain.Invoice {
	invoiceID := s.nextID()
	var customerID uint
	for _, candidate := range s.customers {
		if candidate.CompanyID == companyID && candidate.Name == customer {
			customerID = candidate.ID
			break
		}
	}

	// Split the subtotal into two representative line items so invoice detail
	// views have something meaningful to render.
	firstQty := int64(1)
	firstPrice := subtotal
	lines := []domain.InvoiceLine{
		{
			ID: s.nextID(), InvoiceID: invoiceID,
			Name: "Product & Service Package - " + customer,
			Qty:  firstQty, Price: firstPrice, DiscountPercent: 0, Subtotal: subtotal,
		},
	}

	return domain.Invoice{
		ID: invoiceID, CompanyID: companyID, BranchID: branchID,
		Number: number, CustomerID: customerID, CustomerName: customer,
		Date: date, DueDate: due, Status: status,
		Subtotal: subtotal, TaxAmount: taxAmount, Total: subtotal + taxAmount,
		Notes:     "Payment terms as agreed. Payment via bank transfer.",
		Lines:     lines,
		CreatedAt: time.Now(),
	}
}

// --- Auth & tenants ---

func (s *MemoryStore) FindUserByEmail(email string) (*domain.User, bool) {
	s.rLock()
	defer s.mu.RUnlock()
	u, ok := s.users[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, false
	}
	copy := *u
	return &copy, true
}

func (s *MemoryStore) FindUserByID(id uint) (*domain.User, bool) {
	s.rLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.ID == id {
			copy := *u
			return &copy, true
		}
	}
	return nil, false
}

func (s *MemoryStore) CreateUser(user *domain.User) error {
	s.lock()
	defer s.mu.Unlock()
	email := strings.ToLower(strings.TrimSpace(user.Email))
	if _, exists := s.users[email]; exists {
		return domain.ErrConflict(domain.CodeDuplicate, "email is already registered")
	}
	user.Email = email
	user.ID = s.nextID()
	copy := *user
	s.users[email] = &copy
	return nil
}

func (s *MemoryStore) UpdateUser(user *domain.User) error {
	s.lock()
	defer s.mu.Unlock()
	for email, u := range s.users {
		if u.ID == user.ID {
			copy := *user
			s.users[email] = &copy
			return nil
		}
	}
	return domain.ErrNotFound("user not found")
}

func (s *MemoryStore) CreateSession(session *domain.Session) error {
	s.lock()
	defer s.mu.Unlock()
	copy := *session
	s.sessions[session.TokenHash] = &copy
	return nil
}

func (s *MemoryStore) GetSession(tokenHash string, now time.Time) (*domain.Session, bool) {
	s.lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[tokenHash]
	if !ok || session.RevokedAt != nil || !now.Before(session.ExpiresAt) {
		if ok && !now.Before(session.ExpiresAt) {
			delete(s.sessions, tokenHash)
		}
		return nil, false
	}
	copy := *session
	return &copy, true
}

func (s *MemoryStore) RevokeSession(tokenHash string, at time.Time) error {
	s.lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[tokenHash]; ok {
		session.RevokedAt = &at
	}
	return nil
}

func (s *MemoryStore) DeleteExpiredSessions(now time.Time) error {
	s.lock()
	defer s.mu.Unlock()
	for hash, session := range s.sessions {
		if !now.Before(session.ExpiresAt) || session.RevokedAt != nil {
			delete(s.sessions, hash)
		}
	}
	return nil
}

func (s *MemoryStore) CreatePasswordResetToken(token *domain.PasswordResetToken) error {
	s.lock()
	defer s.mu.Unlock()
	copy := *token
	s.resetTokens[token.TokenHash] = &copy
	return nil
}

// ResetPassword consumes a reset token and sets a new password. It returns the
// user it acted on so the caller can revoke that user's live sessions.
func (s *MemoryStore) ResetPassword(tokenHash, passwordHash string, now time.Time) (uint, error) {
	s.lock()
	defer s.mu.Unlock()
	token, ok := s.resetTokens[tokenHash]
	if !ok || token.UsedAt != nil || !now.Before(token.ExpiresAt) {
		return 0, domain.ErrUnauthorized("password reset token is invalid or expired")
	}
	for email, user := range s.users {
		if user.ID == token.UserID {
			user.Password = passwordHash
			s.users[email] = user
			token.UsedAt = &now
			return user.ID, nil
		}
	}
	return 0, domain.ErrUnauthorized("password reset token is invalid or expired")
}

func (s *MemoryStore) GetCompany(id uint) (*domain.Company, error) {
	s.rLock()
	defer s.mu.RUnlock()
	c, ok := s.companies[id]
	if !ok {
		return nil, domain.ErrNotFound("company not found")
	}
	copy := *c
	return &copy, nil
}

func (s *MemoryStore) CreateCompany(company *domain.Company) error {
	s.lock()
	defer s.mu.Unlock()
	company.ID = s.nextID()
	copy := *company
	s.companies[company.ID] = &copy
	return nil
}

func (s *MemoryStore) UpdateCompany(company *domain.Company) error {
	s.lock()
	defer s.mu.Unlock()
	if _, ok := s.companies[company.ID]; !ok {
		return domain.ErrNotFound("company not found")
	}
	copy := *company
	s.companies[company.ID] = &copy
	return nil
}

func (s *MemoryStore) ListBranches(companyID uint) []domain.Branch {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.Branch, 0)
	for _, b := range s.branches {
		if b.CompanyID == companyID {
			out = append(out, b)
		}
	}
	return out
}

func (s *MemoryStore) CreateBranch(branch *domain.Branch) error {
	s.lock()
	defer s.mu.Unlock()
	branch.ID = s.nextID()
	s.branches = append(s.branches, *branch)
	return nil
}

func (s *MemoryStore) GetBranch(companyID, id uint) (*domain.Branch, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.branches {
		if s.branches[i].CompanyID == companyID && s.branches[i].ID == id {
			b := s.branches[i]
			return &b, nil
		}
	}
	return nil, domain.ErrNotFound("branch not found")
}

func (s *MemoryStore) UpdateBranch(branch *domain.Branch) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.branches {
		if s.branches[i].CompanyID == branch.CompanyID && s.branches[i].ID == branch.ID {
			s.branches[i] = *branch
			return nil
		}
	}
	return domain.ErrNotFound("branch not found")
}

func (s *MemoryStore) DeleteBranch(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.branches {
		if s.branches[i].CompanyID == companyID && s.branches[i].ID == id {
			s.branches = append(s.branches[:i], s.branches[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound("branch not found")
}

// --- Accounts ---

func (s *MemoryStore) ListAccounts(companyID uint) []domain.Account {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.Account, 0)
	for _, a := range s.accounts {
		if a.CompanyID == companyID {
			a.Balance = s.accountBalanceLocked(a)
			out = append(out, a)
		}
	}
	return out
}

func (s *MemoryStore) GetAccount(companyID, id uint) (*domain.Account, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.accounts {
		if s.accounts[i].CompanyID == companyID && s.accounts[i].ID == id {
			a := s.accounts[i]
			a.Balance = s.accountBalanceLocked(a)
			return &a, nil
		}
	}
	return nil, domain.ErrNotFound("account not found")
}

func (s *MemoryStore) GetAccountByCode(companyID uint, code string) (*domain.Account, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.accounts {
		if s.accounts[i].CompanyID == companyID && s.accounts[i].Code == code {
			a := s.accounts[i]
			a.Balance = s.accountBalanceLocked(a)
			return &a, nil
		}
	}
	return nil, domain.ErrNotFound("account not found")
}

func (s *MemoryStore) CreateAccount(account *domain.Account) error {
	s.lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.CompanyID == account.CompanyID && a.Code == account.Code {
			return domain.ErrConflict(domain.CodeDuplicate, "account code is already in use")
		}
	}
	account.ID = s.nextID()
	s.accounts = append(s.accounts, *account)
	return nil
}

func (s *MemoryStore) UpdateAccount(account *domain.Account) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].CompanyID == account.CompanyID && s.accounts[i].ID == account.ID {
			s.accounts[i] = *account
			return nil
		}
	}
	return domain.ErrNotFound("account not found")
}

func (s *MemoryStore) DeleteAccount(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.accounts {
		if s.accounts[i].CompanyID == companyID && s.accounts[i].ID == id {
			if s.accounts[i].IsSystem {
				return domain.ErrForbidden("system accounts cannot be deleted")
			}
			if s.accountBalanceLocked(s.accounts[i]) != 0 {
				return domain.ErrValidation("an account with a balance cannot be deleted")
			}
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound("account not found")
}

func (s *MemoryStore) accountBalanceLocked(account domain.Account) int64 {
	var balance int64
	for _, entry := range s.journals {
		if entry.CompanyID != account.CompanyID || entry.Status != domain.JournalStatusPosted {
			continue
		}
		for _, line := range entry.Lines {
			if line.AccountID == account.ID {
				balance += balanceDeltaFor(account.Type, line.Debit, line.Credit)
			}
		}
	}
	return balance
}

// --- Journal entries ---

func (s *MemoryStore) ListJournalEntries(companyID uint, status string) []domain.JournalEntry {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.JournalEntry, 0)
	for _, e := range s.journals {
		if e.CompanyID != companyID {
			continue
		}
		if status != "" && e.Status != status {
			continue
		}
		out = append(out, e)
	}
	// Newest first, matching the UI.
	sort.Slice(out, func(i, j int) bool { return out[i].Number > out[j].Number })
	return out
}

func (s *MemoryStore) GetJournalEntry(companyID, id uint) (*domain.JournalEntry, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.journals {
		if s.journals[i].CompanyID == companyID && s.journals[i].ID == id {
			e := s.journals[i]
			return &e, nil
		}
	}
	return nil, domain.ErrNotFound("journal entry not found")
}

func (s *MemoryStore) CreateJournalEntry(entry *domain.JournalEntry) error {
	s.lock()
	defer s.mu.Unlock()
	entry.ID = s.nextID()
	for i := range entry.Lines {
		entry.Lines[i].ID = s.nextID()
		entry.Lines[i].JournalEntryID = entry.ID
	}
	s.journals = append(s.journals, *entry)
	return nil
}

func (s *MemoryStore) UpdateJournalEntry(entry *domain.JournalEntry) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.journals {
		if s.journals[i].CompanyID != entry.CompanyID || s.journals[i].ID != entry.ID {
			continue
		}
		if s.journals[i].Status != domain.JournalStatusDraft {
			return domain.ErrConflict(domain.CodeImmutable, "posted journals are immutable")
		}
		for j := range entry.Lines {
			entry.Lines[j].ID = s.nextID()
			entry.Lines[j].JournalEntryID = entry.ID
		}
		s.journals[i] = *entry
		return nil
	}
	return domain.ErrNotFound("journal entry not found")
}

func (s *MemoryStore) PostJournalEntry(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.journals {
		if s.journals[i].CompanyID != companyID || s.journals[i].ID != id {
			continue
		}
		if s.journals[i].Status != domain.JournalStatusDraft {
			return domain.ErrConflict(domain.CodeImmutable, "journal entry has already been posted")
		}
		s.journals[i].Status = domain.JournalStatusPosted
		return nil
	}
	return domain.ErrNotFound("journal entry not found")
}

func (s *MemoryStore) DeleteJournalEntry(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.journals {
		if s.journals[i].CompanyID != companyID || s.journals[i].ID != id {
			continue
		}
		if s.journals[i].Status != domain.JournalStatusDraft {
			return domain.ErrConflict(domain.CodeImmutable, "posted journals cannot be deleted")
		}
		s.journals = append(s.journals[:i], s.journals[i+1:]...)
		return nil
	}
	return domain.ErrNotFound("journal entry not found")
}

func (s *MemoryStore) ListLedgerEntries(companyID, accountID uint) []domain.LedgerEntry {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.LedgerEntry, 0)
	for _, e := range s.journals {
		if e.CompanyID != companyID || e.Status != domain.JournalStatusPosted {
			continue
		}
		for _, l := range e.Lines {
			if l.AccountID != accountID {
				continue
			}
			out = append(out, domain.LedgerEntry{
				Date: e.Date, Ref: e.Number, Memo: firstNonEmpty(l.Memo, e.Memo),
				Debit: l.Debit, Credit: l.Credit,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out
}

// sequenceKey namespaces a number sequence per company and prefix.
func sequenceKey(companyID uint, prefix string) string {
	return fmt.Sprintf("%d-%s", companyID, prefix)
}

func (s *MemoryStore) NextNumber(companyID uint, prefix string) string {
	s.lock()
	defer s.mu.Unlock()
	key := sequenceKey(companyID, prefix)
	s.numbers[key]++
	return fmt.Sprintf("%s-%04d", prefix, s.numbers[key])
}

// SetNextNumber pins a sequence so the next generated number is value+1.
// Used by the seeder to continue after seeded document numbers.
func (s *MemoryStore) SetNextNumber(companyID uint, prefix string, value int) {
	s.lock()
	defer s.mu.Unlock()
	key := sequenceKey(companyID, prefix)
	if value > s.numbers[key] {
		s.numbers[key] = value
	}
}

// appendAudit records a mutation. Callers already hold no lock, so this
// acquires it directly and is safe to call from seed-time code.
func (s *MemoryStore) appendAudit(companyID, userID uint, action, entity, detail string) {
	s.appendAuditAt(companyID, userID, action, entity, detail, time.Now())
}

// appendAuditAt records an audit event at an explicit instant. Seeded events
// are stamped at the originating document's date so the trail is ordered and
// reads chronologically; live events use the current time.
func (s *MemoryStore) appendAuditAt(companyID, userID uint, action, entity, detail string, at time.Time) {
	s.auditLogs = append(s.auditLogs, domain.AuditLog{
		ID:        s.nextID(),
		CompanyID: companyID,
		UserID:    userID,
		Action:    action,
		Entity:    entity,
		AfterJSON: detail,
		CreatedAt: at,
	})
}

// --- Invoices ---

func (s *MemoryStore) ListInvoices(companyID uint) []domain.Invoice {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.Invoice, 0)
	for _, inv := range s.invoices {
		if inv.CompanyID == companyID {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Number > out[j].Number
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (s *MemoryStore) GetInvoice(companyID, id uint) (*domain.Invoice, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.invoices {
		if s.invoices[i].CompanyID == companyID && s.invoices[i].ID == id {
			inv := s.invoices[i]
			return &inv, nil
		}
	}
	return nil, domain.ErrNotFound("invoice not found")
}

func (s *MemoryStore) CreateInvoice(invoice *domain.Invoice) error {
	s.lock()
	defer s.mu.Unlock()
	invoice.ID = s.nextID()
	for i := range invoice.Lines {
		invoice.Lines[i].ID = s.nextID()
		invoice.Lines[i].InvoiceID = invoice.ID
	}
	s.invoices = append(s.invoices, *invoice)
	return nil
}

func (s *MemoryStore) UpdateInvoice(invoice *domain.Invoice) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.invoices {
		if s.invoices[i].CompanyID == invoice.CompanyID && s.invoices[i].ID == invoice.ID {
			if s.invoices[i].Status != domain.InvoiceStatusDraft || invoice.Status != domain.InvoiceStatusDraft {
				return domain.ErrConflict(domain.CodeImmutable, "only draft invoices can be edited")
			}
			for line := range invoice.Lines {
				if invoice.Lines[line].ID == 0 {
					invoice.Lines[line].ID = s.nextID()
				}
				invoice.Lines[line].InvoiceID = invoice.ID
			}
			s.invoices[i] = *invoice
			return nil
		}
	}
	return domain.ErrNotFound("invoice not found")
}

func (s *MemoryStore) TransitionInvoiceStatus(companyID, id uint, from, to string) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.invoices {
		if s.invoices[i].CompanyID == companyID && s.invoices[i].ID == id {
			if s.invoices[i].Status != from {
				return domain.ErrConflict(domain.CodeImmutable, "invoice status has changed")
			}
			s.invoices[i].Status = to
			return nil
		}
	}
	return domain.ErrNotFound("invoice not found")
}

func (s *MemoryStore) DeleteInvoice(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.invoices {
		if s.invoices[i].CompanyID == companyID && s.invoices[i].ID == id {
			s.invoices = append(s.invoices[:i], s.invoices[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound("invoice not found")
}

func (s *MemoryStore) ListInvoicePayments(companyID, invoiceID uint) []domain.InvoicePayment {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.InvoicePayment, 0)
	for _, payment := range s.payments {
		if payment.CompanyID == companyID && payment.InvoiceID == invoiceID {
			out = append(out, payment)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date == out[j].Date {
			return out[i].ID < out[j].ID
		}
		return out[i].Date < out[j].Date
	})
	return out
}

func (s *MemoryStore) CreateInvoicePayment(payment *domain.InvoicePayment) error {
	s.lock()
	defer s.mu.Unlock()
	payment.ID = s.nextID()
	if payment.CreatedAt.IsZero() {
		payment.CreatedAt = time.Now()
	}
	s.payments = append(s.payments, *payment)
	return nil
}

func (s *MemoryStore) ListCustomers(companyID uint) []domain.Customer {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.Customer, 0)
	for _, c := range s.customers {
		if c.CompanyID == companyID {
			out = append(out, c)
		}
	}
	return out
}

func (s *MemoryStore) GetCustomer(companyID, id uint) (*domain.Customer, error) {
	s.rLock()
	defer s.mu.RUnlock()
	for i := range s.customers {
		if s.customers[i].CompanyID == companyID && s.customers[i].ID == id {
			c := s.customers[i]
			return &c, nil
		}
	}
	return nil, domain.ErrNotFound("customer not found")
}

func (s *MemoryStore) CreateCustomer(customer *domain.Customer) error {
	s.lock()
	defer s.mu.Unlock()
	customer.ID = s.nextID()
	s.customers = append(s.customers, *customer)
	return nil
}

func (s *MemoryStore) UpdateCustomer(customer *domain.Customer) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.customers {
		if s.customers[i].CompanyID == customer.CompanyID && s.customers[i].ID == customer.ID {
			s.customers[i] = *customer
			return nil
		}
	}
	return domain.ErrNotFound("customer not found")
}

func (s *MemoryStore) DeleteCustomer(companyID, id uint) error {
	s.lock()
	defer s.mu.Unlock()
	for i := range s.customers {
		if s.customers[i].CompanyID == companyID && s.customers[i].ID == id {
			s.customers = append(s.customers[:i], s.customers[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound("customer not found")
}

// --- Audit ---

func (s *MemoryStore) AppendAuditLog(log domain.AuditLog) error {
	s.lock()
	defer s.mu.Unlock()
	log.ID = s.nextID()
	s.auditLogs = append(s.auditLogs, log)
	return nil
}

func (s *MemoryStore) ListAuditLogs(companyID uint) []domain.AuditLog {
	s.rLock()
	defer s.mu.RUnlock()
	out := make([]domain.AuditLog, 0)
	for _, l := range s.auditLogs {
		if l.CompanyID == companyID {
			out = append(out, l)
		}
	}
	// Mirror PostgresStore: newest event first, id descending as the
	// tie-breaker. The in-memory store is a first-class deployment target (it
	// backs the no-database quick start), so it must not present a different
	// ordering from the Postgres path.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// --- Optional capabilities ---

// RevokeSessionsForUser ends every session belonging to a user.
//
// Called after a password reset: a bearer token captured before the reset would
// otherwise keep authenticating for the whole session TTL, which is exactly the
// window a stolen token is worth. The rows are deleted rather than flagged,
// mirroring the PostgreSQL implementation and keeping expired-token sweeps cheap.
func (s *MemoryStore) RevokeSessionsForUser(_ context.Context, userID int64) error {
	s.lock()
	defer s.mu.Unlock()
	target := uint(userID)
	for hash, session := range s.sessions {
		if session.UserID == target {
			delete(s.sessions, hash)
		}
	}
	return nil
}

// DeleteExpiredPasswordResetTokens drops spent and expired reset tokens. The
// auth layer calls it opportunistically while issuing a session.
func (s *MemoryStore) DeleteExpiredPasswordResetTokens(_ context.Context, now time.Time) error {
	s.lock()
	defer s.mu.Unlock()
	for hash, token := range s.resetTokens {
		if token.UsedAt != nil || !now.Before(token.ExpiresAt) {
			delete(s.resetTokens, hash)
		}
	}
	return nil
}

// FindUsersByIDs resolves many users in one pass, satisfying UserDirectory so a
// page of audit rows costs one lookup instead of one per row.
func (s *MemoryStore) FindUsersByIDs(ids []uint) map[uint]*domain.User {
	s.rLock()
	defer s.mu.RUnlock()
	found := make(map[uint]*domain.User, len(ids))
	for _, id := range ids {
		for _, user := range s.users {
			if user.ID == id {
				copied := *user
				found[id] = &copied
				break
			}
		}
	}
	return found
}

// The Read* methods satisfy FallibleStore. An in-memory read cannot fail, so
// they delegate to the slice-returning methods and never report an error; they
// exist so a caller can use one code path against both stores.
func (s *MemoryStore) ReadBranches(companyID uint) ([]domain.Branch, error) {
	return s.ListBranches(companyID), nil
}

func (s *MemoryStore) ReadAccounts(companyID uint) ([]domain.Account, error) {
	return s.ListAccounts(companyID), nil
}

func (s *MemoryStore) ReadJournalEntries(companyID uint, status string) ([]domain.JournalEntry, error) {
	return s.ListJournalEntries(companyID, status), nil
}

func (s *MemoryStore) ReadLedgerEntries(companyID, accountID uint) ([]domain.LedgerEntry, error) {
	return s.ListLedgerEntries(companyID, accountID), nil
}

func (s *MemoryStore) ReadInvoices(companyID uint) ([]domain.Invoice, error) {
	return s.ListInvoices(companyID), nil
}

func (s *MemoryStore) ReadInvoicePayments(companyID, invoiceID uint) ([]domain.InvoicePayment, error) {
	return s.ListInvoicePayments(companyID, invoiceID), nil
}

func (s *MemoryStore) ReadCustomers(companyID uint) ([]domain.Customer, error) {
	return s.ListCustomers(companyID), nil
}

func (s *MemoryStore) ReadAuditLogs(companyID uint) ([]domain.AuditLog, error) {
	return s.ListAuditLogs(companyID), nil
}
