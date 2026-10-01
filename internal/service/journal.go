package service

import (
	"encoding/json"
	"strings"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

// JournalService enforces double-entry rules around journal posting
// (PRD §10.4): a journal must balance, posted journals are immutable, and
// every financial mutation is audited.
type JournalService struct {
	store repository.Store
}

func NewJournalService(store repository.Store) *JournalService {
	return &JournalService{store: store}
}

// JournalLineInput is a single line submitted by the client.
type JournalLineInput struct {
	AccountID   uint   `json:"accountId"`
	AccountCode string `json:"accountCode"`
	Debit       int64  `json:"debit"`
	Credit      int64  `json:"credit"`
	Memo        string `json:"memo"`
}

// JournalInput is the client payload for creating a journal entry.
type JournalInput struct {
	Date       string             `json:"date"`
	BranchID   uint               `json:"branchId"`
	Memo       string             `json:"memo"`
	Status     string             `json:"status"`
	SourceType string             `json:"sourceType"`
	SourceID   uint               `json:"sourceId"`
	Lines      []JournalLineInput `json:"lines"`
}

// JournalSummary is the list-view projection of a journal entry.
type JournalSummary struct {
	ID       uint   `json:"id"`
	BranchID uint   `json:"branchId"`
	Number   string `json:"number"`
	Date     string `json:"date"`
	Source   string `json:"source"`
	Memo     string `json:"memo"`
	Status   string `json:"status"`
	Total    int64  `json:"total"`
}

// List returns journal summaries for a company, optionally filtered by status.
func (s *JournalService) List(companyID uint, status string) []JournalSummary {
	entries := s.store.ListJournalEntries(companyID, status)
	out := make([]JournalSummary, 0, len(entries))
	for _, e := range entries {
		out = append(out, JournalSummary{
			ID: e.ID, BranchID: e.BranchID, Number: e.Number, Date: e.Date, Source: e.SourceType,
			Memo: e.Memo, Status: e.Status, Total: entryTotal(e),
		})
	}
	return out
}

// Get returns a full journal entry with its lines.
func (s *JournalService) Get(companyID, id uint) (*domain.JournalEntry, error) {
	return s.store.GetJournalEntry(companyID, id)
}

// Create validates and stores a journal entry. New entries default to Draft;
// callers that need an immediately posted system journal must request Posted.
func (s *JournalService) Create(companyID, userID uint, input JournalInput) (*domain.JournalEntry, error) {
	entry, totalDebit, err := s.buildEntry(companyID, input)
	if err != nil {
		return nil, err
	}
	entry.CreatedAt = time.Now()

	after, _ := json.Marshal(map[string]any{"number": entry.Number, "memo": entry.Memo, "total": totalDebit})
	action := "journal.create"
	if entry.Status == domain.JournalStatusPosted {
		action = "journal.post"
	}
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		entry.Number = tx.NextNumber(companyID, "JRN")
		if err := tx.CreateJournalEntry(entry); err != nil {
			return err
		}
		after, _ = json.Marshal(map[string]any{"number": entry.Number, "memo": entry.Memo, "total": totalDebit})
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: action,
			Entity: "journal_entry", AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}

	return entry, nil
}

// Update replaces a draft journal. Posted journals are immutable.
func (s *JournalService) Update(companyID, userID, id uint, input JournalInput) (*domain.JournalEntry, error) {
	current, err := s.store.GetJournalEntry(companyID, id)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.JournalStatusDraft {
		return nil, domain.ErrConflict(domain.CodeImmutable, "posted journals are immutable")
	}
	if input.Status != "" && input.Status != domain.JournalStatusDraft {
		return nil, domain.ErrValidation("a draft must be posted through the post action")
	}
	input.Status = domain.JournalStatusDraft
	entry, total, err := s.buildEntry(companyID, input)
	if err != nil {
		return nil, err
	}
	entry.ID = current.ID
	entry.Number = current.Number
	entry.CreatedAt = current.CreatedAt
	before, _ := json.Marshal(map[string]any{"memo": current.Memo, "total": entryTotal(*current)})
	after, _ := json.Marshal(map[string]any{"memo": entry.Memo, "total": total})
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		if err := tx.UpdateJournalEntry(entry); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "journal.update",
			Entity: "journal_entry", BeforeJSON: string(before), AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}
	return entry, nil
}

// Post transitions a draft exactly once. Posted journals are immutable.
func (s *JournalService) Post(companyID, userID, id uint) (*domain.JournalEntry, error) {
	entry, err := s.store.GetJournalEntry(companyID, id)
	if err != nil {
		return nil, err
	}
	if entry.Status != domain.JournalStatusDraft {
		return nil, domain.ErrConflict(domain.CodeImmutable, "journal entry has already been posted")
	}
	entry.Status = domain.JournalStatusPosted
	after, _ := json.Marshal(map[string]any{"number": entry.Number, "total": entryTotal(*entry)})
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		if err := tx.PostJournalEntry(companyID, id); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "journal.post",
			Entity: "journal_entry", AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}
	return entry, nil
}

// Delete removes a draft journal. Posted journals must remain in the ledger.
func (s *JournalService) Delete(companyID, userID, id uint) error {
	entry, err := s.store.GetJournalEntry(companyID, id)
	if err != nil {
		return err
	}
	if entry.Status != domain.JournalStatusDraft {
		return domain.ErrConflict(domain.CodeImmutable, "posted journals cannot be deleted")
	}
	before, _ := json.Marshal(map[string]any{"number": entry.Number, "memo": entry.Memo})
	return s.store.WithTransaction(func(tx repository.Store) error {
		if err := tx.DeleteJournalEntry(companyID, id); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "journal.delete",
			Entity: "journal_entry", BeforeJSON: string(before), CreatedAt: time.Now()})
	})
}

func (s *JournalService) buildEntry(companyID uint, input JournalInput) (*domain.JournalEntry, int64, error) {
	if len(input.Lines) == 0 {
		return nil, 0, domain.ErrValidation("a journal must contain at least one line")
	}

	var totalDebit, totalCredit int64
	resolved := make([]domain.JournalLine, 0, len(input.Lines))
	for _, l := range input.Lines {
		account, err := s.resolveAccount(companyID, l)
		if err != nil {
			return nil, 0, err
		}
		if l.Debit < 0 || l.Credit < 0 {
			return nil, 0, domain.ErrValidation("debit and credit cannot be negative")
		}
		if !account.IsActive {
			return nil, 0, domain.ErrValidation("inactive accounts cannot be used in a journal")
		}
		if l.Debit == 0 && l.Credit == 0 {
			return nil, 0, domain.ErrValidation("each line must have a debit or credit amount")
		}
		if l.Debit > 0 && l.Credit > 0 {
			return nil, 0, domain.ErrValidation("a line cannot contain both a debit and a credit")
		}
		totalDebit += l.Debit
		totalCredit += l.Credit
		resolved = append(resolved, domain.JournalLine{
			AccountID: account.ID, Debit: l.Debit, Credit: l.Credit, Memo: l.Memo,
		})
	}

	if totalDebit != totalCredit {
		return nil, 0, domain.ErrConflict(domain.CodeJournalNotBalanced, "journal debits and credits must balance")
	}
	if totalDebit == 0 {
		return nil, 0, domain.ErrValidation("journal total cannot be zero")
	}

	status := strings.TrimSpace(input.Status)
	if status == "" {
		status = domain.JournalStatusDraft
	}
	if status != domain.JournalStatusPosted && status != domain.JournalStatusDraft {
		return nil, 0, domain.ErrValidation("invalid journal status")
	}

	sourceType := strings.TrimSpace(input.SourceType)
	if sourceType == "" {
		sourceType = domain.SourceManual
	}
	if sourceType != domain.SourceManual && sourceType != domain.SourceInvoice && sourceType != domain.SourcePayment {
		return nil, 0, domain.ErrValidation("invalid journal source")
	}

	date, _, err := normalizeDate(strings.TrimSpace(input.Date), time.Now())
	if err != nil {
		return nil, 0, err
	}
	branchID, err := resolveBranch(s.store, companyID, input.BranchID)
	if err != nil {
		return nil, 0, err
	}

	entry := &domain.JournalEntry{
		CompanyID:  companyID,
		BranchID:   branchID,
		Date:       date,
		Status:     status,
		SourceType: sourceType,
		SourceID:   input.SourceID,
		Memo:       strings.TrimSpace(input.Memo),
		Lines:      resolved,
	}
	return entry, totalDebit, nil
}

func (s *JournalService) resolveAccount(companyID uint, l JournalLineInput) (*domain.Account, error) {
	if l.AccountID != 0 {
		return s.store.GetAccount(companyID, l.AccountID)
	}
	if l.AccountCode != "" {
		return s.store.GetAccountByCode(companyID, l.AccountCode)
	}
	return nil, domain.ErrValidation("each line must reference an account (accountId or accountCode)")
}

// balanceDelta returns the signed change to an account balance given a
// debit/credit line and the account's normal balance side.
func balanceDelta(accountType string, debit, credit int64) int64 {
	switch accountType {
	case domain.TypeAsset, domain.TypeExpense:
		return debit - credit
	default: // liability, equity, revenue
		return credit - debit
	}
}

func entryTotal(e domain.JournalEntry) int64 {
	var total int64
	for _, l := range e.Lines {
		total += l.Debit
	}
	return total
}
