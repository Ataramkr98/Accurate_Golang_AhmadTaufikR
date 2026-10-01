package service

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

// InvoiceService creates invoices and publishes them, which auto-generates
// the corresponding double-entry journal (Debit Piutang, Kredit Pendapatan)
// plus a PPN tax transaction.
type InvoiceService struct {
	store   repository.Store
	tax     *TaxService
	journal *JournalService
}

func NewInvoiceService(store repository.Store, tax *TaxService, journal *JournalService) *InvoiceService {
	return &InvoiceService{store: store, tax: tax, journal: journal}
}

// InvoiceLineInput is a single invoice line submitted by the client.
type InvoiceLineInput struct {
	Name            string `json:"name"`
	Qty             int64  `json:"qty"`
	Price           int64  `json:"price"`
	DiscountPercent int64  `json:"discountPercent"`
}

// InvoiceInput is the client payload for creating an invoice.
type InvoiceInput struct {
	CustomerID   uint               `json:"customerId"`
	CustomerName string             `json:"customerName"`
	Date         string             `json:"date"`
	DueDate      string             `json:"dueDate"`
	Notes        string             `json:"notes"`
	BranchID     uint               `json:"branchId"`
	Lines        []InvoiceLineInput `json:"lines"`
}

func (s *InvoiceService) List(companyID uint) []domain.Invoice {
	invoices := s.store.ListInvoices(companyID)
	for i := range invoices {
		s.enrichInvoice(&invoices[i])
	}
	return invoices
}

func (s *InvoiceService) Get(companyID, id uint) (*domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(companyID, id)
	if err != nil {
		return nil, err
	}
	s.enrichInvoice(invoice)
	return invoice, nil
}

// UpdateDraft replaces a draft document before it is issued. Issued invoices
// remain immutable because their amounts are represented by posted journals.
func (s *InvoiceService) UpdateDraft(companyID, userID, id uint, input InvoiceInput) (*domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(companyID, id)
	if err != nil {
		return nil, err
	}
	if invoice.Status != domain.InvoiceStatusDraft {
		return nil, domain.ErrConflict(domain.CodeImmutable, "issued invoices cannot be edited")
	}
	draft, err := s.buildDraft(companyID, input)
	if err != nil {
		return nil, err
	}
	before, _ := json.Marshal(map[string]any{"number": invoice.Number, "total": invoice.Total})
	invoice.BranchID = draft.BranchID
	invoice.Date = draft.Date
	invoice.DueDate = draft.DueDate
	invoice.Notes = draft.Notes
	invoice.Subtotal = draft.Subtotal
	invoice.TaxAmount = draft.TaxAmount
	invoice.Total = draft.Total
	invoice.Lines = draft.Lines
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		customerID, customerName, err := resolveInvoiceCustomer(tx, companyID, input)
		if err != nil {
			return err
		}
		invoice.CustomerID = customerID
		invoice.CustomerName = customerName
		if err := tx.UpdateInvoice(invoice); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"number": invoice.Number, "total": invoice.Total})
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "invoice.update",
			Entity: "invoice", BeforeJSON: string(before), AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}
	s.enrichInvoice(invoice)
	return invoice, nil
}

func (s *InvoiceService) Delete(companyID, userID, id uint) error {
	invoice, err := s.store.GetInvoice(companyID, id)
	if err != nil {
		return err
	}
	if invoice.Status != domain.InvoiceStatusDraft {
		return domain.ErrConflict(domain.CodeImmutable, "issued invoices cannot be deleted")
	}
	before, _ := json.Marshal(map[string]any{"number": invoice.Number, "total": invoice.Total})
	return s.store.WithTransaction(func(tx repository.Store) error {
		if err := tx.DeleteInvoice(companyID, id); err != nil {
			return err
		}
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "invoice.delete",
			Entity: "invoice", BeforeJSON: string(before), CreatedAt: time.Now()})
	})
}

func (s *InvoiceService) ListCustomers(companyID uint) []domain.Customer {
	return s.store.ListCustomers(companyID)
}

func (s *InvoiceService) GetCustomer(companyID, id uint) (*domain.Customer, error) {
	return s.store.GetCustomer(companyID, id)
}

func (s *InvoiceService) CreateCustomer(companyID uint, c domain.Customer) (*domain.Customer, error) {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return nil, domain.ErrValidation("customer name is required")
	}
	c.CompanyID = companyID
	if err := s.store.CreateCustomer(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *InvoiceService) UpdateCustomer(companyID, id uint, c domain.Customer) (*domain.Customer, error) {
	existing, err := s.store.GetCustomer(companyID, id)
	if err != nil {
		return nil, err
	}
	if name := strings.TrimSpace(c.Name); name != "" {
		existing.Name = name
	}
	existing.Email = strings.TrimSpace(c.Email)
	existing.Phone = strings.TrimSpace(c.Phone)
	existing.Address = strings.TrimSpace(c.Address)
	if err := s.store.UpdateCustomer(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func (s *InvoiceService) DeleteCustomer(companyID, id uint) error {
	return s.store.DeleteCustomer(companyID, id)
}

func (s *InvoiceService) Create(companyID, userID uint, input InvoiceInput) (*domain.Invoice, error) {
	invoice, err := s.buildDraft(companyID, input)
	if err != nil {
		return nil, err
	}
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		customerID, customerName, err := resolveInvoiceCustomer(tx, companyID, input)
		if err != nil {
			return err
		}
		invoice.CustomerID = customerID
		invoice.CustomerName = customerName
		invoice.Number = tx.NextNumber(companyID, "INV")
		if err := tx.CreateInvoice(invoice); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"number": invoice.Number, "total": invoice.Total})
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "invoice.create",
			Entity: "invoice", AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}

	s.enrichInvoice(invoice)
	return invoice, nil
}

func (s *InvoiceService) buildDraft(companyID uint, input InvoiceInput) (*domain.Invoice, error) {
	if input.CustomerID == 0 && strings.TrimSpace(input.CustomerName) == "" {
		return nil, domain.ErrValidation("a customer must be selected")
	}
	if len(input.Lines) == 0 {
		return nil, domain.ErrValidation("an invoice needs at least one line")
	}
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	date, parsedDate, err := normalizeDate(strings.TrimSpace(input.Date), time.Now())
	if err != nil {
		return nil, err
	}
	dueDate, parsedDueDate, err := normalizeDate(strings.TrimSpace(input.DueDate), parsedDate.AddDate(0, 0, 30))
	if err != nil {
		return nil, err
	}
	if parsedDueDate.Before(parsedDate) {
		return nil, domain.ErrValidation("the due date cannot be before the invoice date")
	}
	branchID, err := resolveBranch(s.store, companyID, input.BranchID)
	if err != nil {
		return nil, err
	}
	var subtotal int64
	lines := make([]domain.InvoiceLine, 0, len(input.Lines))
	for _, line := range input.Lines {
		name := strings.TrimSpace(line.Name)
		if name == "" || line.Qty <= 0 || line.Price <= 0 {
			return nil, domain.ErrValidation("each line needs a description, positive quantity, and positive price")
		}
		if line.DiscountPercent < 0 || line.DiscountPercent > 100 {
			return nil, domain.ErrValidation("discount must be between 0 and 100 percent")
		}
		if line.Qty > math.MaxInt64/line.Price {
			return nil, domain.ErrValidation("invoice line value is too large")
		}
		base := line.Qty * line.Price
		lineSubtotal := base - (base * line.DiscountPercent / 100)
		if subtotal > math.MaxInt64-lineSubtotal {
			return nil, domain.ErrValidation("invoice total is too large")
		}
		subtotal += lineSubtotal
		lines = append(lines, domain.InvoiceLine{Name: name, Qty: line.Qty, Price: line.Price,
			DiscountPercent: line.DiscountPercent, Subtotal: lineSubtotal})
	}
	if subtotal > math.MaxInt64/12 {
		return nil, domain.ErrValidation("invoice total is too large")
	}
	ppn := PPNResult{DPP: subtotal, Total: subtotal}
	if company.IsPKP {
		ppn = s.tax.CalculatePPN(subtotal)
	}
	return &domain.Invoice{CompanyID: companyID, BranchID: branchID, CustomerName: strings.TrimSpace(input.CustomerName),
		Date: date, DueDate: dueDate, Status: domain.InvoiceStatusDraft, Notes: strings.TrimSpace(input.Notes),
		Subtotal: subtotal, TaxAmount: ppn.TaxAmount, Total: ppn.Total, Lines: lines, CreatedAt: time.Now()}, nil
}

// Issue posts the receivable, revenue, and tax journal for a draft invoice.
func (s *InvoiceService) Issue(companyID, userID, id uint) (*domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(companyID, id)
	if err != nil {
		return nil, err
	}
	if invoice.Status != domain.InvoiceStatusDraft {
		return nil, domain.ErrConflict(domain.CodeImmutable, "invoice has already been issued")
	}
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	posting, err := s.resolvePostingAccounts(companyID, company.IsPKP)
	if err != nil {
		return nil, err
	}
	var journal *domain.JournalEntry
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		transactionalJournal := NewJournalService(tx)
		var err error
		journal, err = postInvoiceJournal(transactionalJournal, companyID, userID, invoice, posting)
		if err != nil {
			return err
		}
		if err := tx.TransitionInvoiceStatus(companyID, invoice.ID, domain.InvoiceStatusDraft, domain.InvoiceStatusIssued); err != nil {
			return err
		}
		invoice.Status = domain.InvoiceStatusIssued
		after, _ := json.Marshal(map[string]any{"number": invoice.Number, "journalId": journal.ID, "total": invoice.Total})
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "invoice.issue",
			Entity: "invoice", AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}
	s.enrichInvoice(invoice)
	return invoice, nil
}

type InvoicePaymentInput struct {
	Date          string `json:"date"`
	Amount        int64  `json:"amount"`
	CashAccountID uint   `json:"cashAccountId"`
	Notes         string `json:"notes"`
}

func (s *InvoiceService) ListPayments(companyID, invoiceID uint) ([]domain.InvoicePayment, error) {
	if _, err := s.store.GetInvoice(companyID, invoiceID); err != nil {
		return nil, err
	}
	return s.store.ListInvoicePayments(companyID, invoiceID), nil
}

// RecordPayment posts a cash receipt and derives invoice state from the
// remaining amount. Payments cannot exceed the outstanding receivable.
func (s *InvoiceService) RecordPayment(companyID, userID, invoiceID uint, input InvoicePaymentInput) (*domain.Invoice, error) {
	invoice, err := s.store.GetInvoice(companyID, invoiceID)
	if err != nil {
		return nil, err
	}
	previousStatus := invoice.Status
	s.enrichInvoice(invoice)
	if invoice.Status == domain.InvoiceStatusDraft {
		return nil, domain.ErrConflict(domain.CodeImmutable, "a draft invoice cannot receive payments")
	}
	if invoice.Outstanding == 0 {
		return nil, domain.ErrConflict(domain.CodeImmutable, "invoice is already paid")
	}
	if input.Amount <= 0 || input.Amount > invoice.Outstanding {
		return nil, domain.ErrValidation("payment must be positive and cannot exceed the outstanding amount")
	}
	cash, err := s.store.GetAccount(companyID, input.CashAccountID)
	if err != nil {
		return nil, err
	}
	if !cash.IsActive || cash.Type != domain.TypeAsset {
		return nil, domain.ErrValidation("payment account must be an active asset account")
	}
	receivable, err := s.store.GetAccountByCode(companyID, "1200")
	if err != nil {
		return nil, err
	}
	date, _, err := normalizeDate(strings.TrimSpace(input.Date), time.Now())
	if err != nil {
		return nil, err
	}
	invoice.PaidAmount += input.Amount
	invoice.Outstanding -= input.Amount
	if invoice.Outstanding == 0 {
		invoice.Status = domain.InvoiceStatusPaid
	} else {
		invoice.Status = domain.InvoiceStatusPartiallyPaid
	}
	if err := s.store.WithTransaction(func(tx repository.Store) error {
		journal, err := NewJournalService(tx).Create(companyID, userID, JournalInput{
			Date: date, BranchID: invoice.BranchID, Memo: "Payment received - " + invoice.Number,
			SourceType: domain.SourceInvoice, SourceID: invoice.ID, Status: domain.JournalStatusPosted,
			Lines: []JournalLineInput{{AccountID: cash.ID, Debit: input.Amount}, {AccountID: receivable.ID, Credit: input.Amount}},
		})
		if err != nil {
			return err
		}
		payment := &domain.InvoicePayment{CompanyID: companyID, InvoiceID: invoice.ID,
			CashAccountID: cash.ID, Date: date, Amount: input.Amount, Notes: strings.TrimSpace(input.Notes),
			JournalID: journal.ID, CreatedAt: time.Now()}
		if err := tx.CreateInvoicePayment(payment); err != nil {
			return err
		}
		if err := tx.TransitionInvoiceStatus(companyID, invoice.ID, previousStatus, invoice.Status); err != nil {
			return err
		}
		after, _ := json.Marshal(map[string]any{"invoice": invoice.Number, "amount": payment.Amount, "journalId": journal.ID})
		return tx.AppendAuditLog(domain.AuditLog{CompanyID: companyID, UserID: userID, Action: "invoice.payment",
			Entity: "invoice_payment", AfterJSON: string(after), CreatedAt: time.Now()})
	}); err != nil {
		return nil, err
	}
	return invoice, nil
}

func (s *InvoiceService) enrichInvoice(invoice *domain.Invoice) {
	var paid int64
	for _, payment := range s.store.ListInvoicePayments(invoice.CompanyID, invoice.ID) {
		paid += payment.Amount
	}
	// Legacy seeded paid invoices predate payment records. The versioned seed
	// migration replaces these with explicit receipts; this keeps upgrades sane.
	if invoice.Status == domain.InvoiceStatusPaid && paid == 0 {
		paid = invoice.Total
	}
	invoice.PaidAmount = paid
	invoice.Outstanding = invoice.Total - paid
	if invoice.Outstanding < 0 {
		invoice.Outstanding = 0
	}
	if invoice.Status != domain.InvoiceStatusDraft && invoice.Outstanding > 0 && invoice.DueDate < time.Now().Format("2006-01-02") {
		invoice.Status = domain.InvoiceStatusOverdue
	}
}

func resolveInvoiceCustomer(store repository.Store, companyID uint, input InvoiceInput) (uint, string, error) {
	if input.CustomerID != 0 {
		customer, err := store.GetCustomer(companyID, input.CustomerID)
		if err != nil {
			return 0, "", err
		}
		return customer.ID, customer.Name, nil
	}
	name := strings.TrimSpace(input.CustomerName)
	for _, c := range store.ListCustomers(companyID) {
		if strings.EqualFold(c.Name, name) {
			return c.ID, c.Name, nil
		}
	}
	c := &domain.Customer{CompanyID: companyID, Name: name}
	if err := store.CreateCustomer(c); err != nil {
		return 0, "", err
	}
	return c.ID, c.Name, nil
}

type invoicePostingAccounts struct {
	receivable uint
	revenue    uint
	ppn        uint
}

func (s *InvoiceService) resolvePostingAccounts(companyID uint, includePPN bool) (invoicePostingAccounts, error) {
	receivable, err := s.store.GetAccountByCode(companyID, "1200") // Piutang Usaha
	if err != nil {
		return invoicePostingAccounts{}, err
	}
	revenue, err := s.store.GetAccountByCode(companyID, "4100") // Penjualan Produk
	if err != nil {
		return invoicePostingAccounts{}, err
	}
	posting := invoicePostingAccounts{receivable: receivable.ID, revenue: revenue.ID}
	if includePPN {
		ppnAccount, err := s.store.GetAccountByCode(companyID, "2200") // Utang Pajak PPN
		if err != nil {
			return invoicePostingAccounts{}, err
		}
		posting.ppn = ppnAccount.ID
	}
	return posting, nil
}

func postInvoiceJournal(journal *JournalService, companyID, userID uint, inv *domain.Invoice, posting invoicePostingAccounts) (*domain.JournalEntry, error) {
	lines := []JournalLineInput{
		{AccountID: posting.receivable, Debit: inv.Total},
		{AccountID: posting.revenue, Credit: inv.Subtotal},
	}
	if inv.TaxAmount > 0 {
		lines = append(lines, JournalLineInput{AccountID: posting.ppn, Credit: inv.TaxAmount})
	}

	entry, err := journal.Create(companyID, userID, JournalInput{
		Date:       inv.Date,
		BranchID:   inv.BranchID,
		Memo:       "Sale - " + inv.Number,
		SourceType: domain.SourceInvoice,
		SourceID:   inv.ID,
		Status:     domain.JournalStatusPosted,
		Lines:      lines,
	})
	return entry, err
}
