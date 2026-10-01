package domain

import "time"

// Session stores only a hash of the opaque bearer token.
type Session struct {
	TokenHash string     `json:"-"`
	UserID    uint       `json:"userId"`
	CompanyID uint       `json:"companyId"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
}

type PasswordResetToken struct {
	TokenHash string
	UserID    uint
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Account types (Chart of Accounts classification).
const (
	TypeAsset     = "asset"
	TypeLiability = "liability"
	TypeEquity    = "equity"
	TypeRevenue   = "revenue"
	TypeExpense   = "expense"

	// Display labels used by the frontend and reports.
	TypeAssetLabel     = "Asset"
	TypeLiabilityLabel = "Liability"
	TypeEquityLabel    = "Equity"
	TypeRevenueLabel   = "Revenue"
	TypeExpenseLabel   = "Expense"
)

// Journal statuses.
const (
	JournalStatusDraft  = "Draft"
	JournalStatusPosted = "Posted"
)

// Source types for journal entries (polymorphic source reference).
const (
	SourceManual  = "Manual"
	SourceInvoice = "Invoice"
	SourcePayment = "Payment"
)

// Invoice statuses.
const (
	InvoiceStatusDraft         = "Draft"
	InvoiceStatusIssued        = "Issued"
	InvoiceStatusPartiallyPaid = "PartiallyPaid"
	InvoiceStatusPaid          = "Paid"
	InvoiceStatusOverdue       = "Overdue"
)

// SAK reporting modes.
const (
	SAKEMKM = "emkm"
	SAKPSAK = "psak_umum"
)

// Company is the root tenant.
type Company struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	NPWP         string `json:"npwp"`
	SAKMode      string `json:"sakMode"`
	IsPKP        bool   `json:"isPkp"`
	Address      string `json:"address"`
	BusinessType string `json:"businessType"`
}

// Branch is a multi-branch scope under a company.
type Branch struct {
	ID        uint   `json:"id"`
	CompanyID uint   `json:"companyId"`
	Name      string `json:"name"`
}

// User is an account that authenticates against the platform.
type User struct {
	ID        uint   `json:"id"`
	CompanyID uint   `json:"companyId"`
	Email     string `json:"email"`
	Password  string `json:"-"`
	Name      string `json:"name"`
	Role      string `json:"-"`
}

// Account is a Chart of Accounts entry (hierarchical via ParentID).
type Account struct {
	ID        uint   `json:"id"`
	CompanyID uint   `json:"companyId"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	ParentID  uint   `json:"parentId"`
	IsSystem  bool   `json:"isSystem"`
	IsActive  bool   `json:"isActive"`
	Balance   int64  `json:"balance"` // current running balance in rupiah (debit normal for asset/expense)
}

// JournalEntry is a double-entry journal header.
type JournalEntry struct {
	ID         uint          `json:"id"`
	CompanyID  uint          `json:"companyId"`
	BranchID   uint          `json:"branchId"`
	Number     string        `json:"number"`
	Date       string        `json:"date"` // YYYY-MM-DD
	Status     string        `json:"status"`
	SourceType string        `json:"sourceType"`
	SourceID   uint          `json:"sourceId"`
	Memo       string        `json:"memo"`
	Lines      []JournalLine `json:"lines"`
	CreatedAt  time.Time     `json:"createdAt"`
}

// JournalLine is a single debit/credit line of a journal entry.
type JournalLine struct {
	ID             uint   `json:"id"`
	JournalEntryID uint   `json:"journalEntryId"`
	AccountID      uint   `json:"accountId"`
	Debit          int64  `json:"debit"`
	Credit         int64  `json:"credit"`
	Memo           string `json:"memo"`
}

// Customer is an AR counterparty.
type Customer struct {
	ID        uint   `json:"id"`
	CompanyID uint   `json:"companyId"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Address   string `json:"address,omitempty"`
}

// InvoiceLine is one line item of an invoice.
type InvoiceLine struct {
	ID              uint   `json:"id"`
	InvoiceID       uint   `json:"invoiceId"`
	Name            string `json:"name"`
	Qty             int64  `json:"qty"`
	Price           int64  `json:"price"`
	DiscountPercent int64  `json:"discountPercent"`
	Subtotal        int64  `json:"subtotal"`
}

// Invoice is an accounts-receivable sales invoice.
type Invoice struct {
	ID           uint          `json:"id"`
	CompanyID    uint          `json:"companyId"`
	BranchID     uint          `json:"branchId"`
	Number       string        `json:"number"`
	CustomerID   uint          `json:"customerId"`
	CustomerName string        `json:"customerName"`
	Date         string        `json:"date"`
	DueDate      string        `json:"dueDate"`
	Status       string        `json:"status"`
	Notes        string        `json:"notes"`
	Subtotal     int64         `json:"subtotal"`
	TaxAmount    int64         `json:"taxAmount"`
	Total        int64         `json:"total"`
	PaidAmount   int64         `json:"paidAmount"`
	Outstanding  int64         `json:"outstanding"`
	Lines        []InvoiceLine `json:"lines"`
	CreatedAt    time.Time     `json:"createdAt"`
}

// InvoicePayment records a cash receipt against an issued sales invoice.
type InvoicePayment struct {
	ID            uint      `json:"id"`
	CompanyID     uint      `json:"companyId"`
	InvoiceID     uint      `json:"invoiceId"`
	CashAccountID uint      `json:"cashAccountId"`
	Date          string    `json:"date"`
	Amount        int64     `json:"amount"`
	Notes         string    `json:"notes"`
	JournalID     uint      `json:"journalId"`
	CreatedAt     time.Time `json:"createdAt"`
}

// AuditLog is an immutable record of a financial mutation.
type AuditLog struct {
	ID         uint      `json:"id"`
	CompanyID  uint      `json:"companyId"`
	UserID     uint      `json:"userId"`
	Action     string    `json:"action"`
	Entity     string    `json:"entity"`
	BeforeJSON string    `json:"beforeJson"`
	AfterJSON  string    `json:"afterJson"`
	CreatedAt  time.Time `json:"createdAt"`
}

// LedgerEntry is a flattened journal line for a general-ledger view,
// joined with its parent entry's date/number/memo.
type LedgerEntry struct {
	Date   string `json:"date"`
	Ref    string `json:"ref"`
	Memo   string `json:"memo"`
	Debit  int64  `json:"debit"`
	Credit int64  `json:"credit"`
}
