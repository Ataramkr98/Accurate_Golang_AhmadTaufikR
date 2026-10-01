package repository

import (
	"fmt"
	"time"

	"tera/internal/domain"
)

// Demo seed design
// ----------------
// Dates are relative to "today" so the demo never looks stale. More
// importantly, account balances are NOT hardcoded: only opening balances are
// declared, and every later movement comes from a journal entry. Applying the
// journals to the opening balances produces a balance sheet that satisfies
// Assets = Liabilities + Equity, because each journal is itself balanced.
// Hardcoding balances (as an earlier revision did) silently breaks the
// accounting equation and makes the reports untrustworthy.

// dateOffset returns an ISO date `days` away from today (negative = past).
func dateOffset(days int) string {
	return time.Now().AddDate(0, 0, days).Format("2006-01-02")
}

// Demo credentials. Shared by both stores and both seeding paths so the login
// screen, the README, and the seeded users can never drift apart.
const (
	demoEmail    = "demo@tera.co.id"
	demoPassword = "Demo1234"
)

// monthDay returns an ISO date inside the month `months` back from the current
// one, on the given day-of-month (clamped to that month's length so 31 always
// resolves, e.g. to 28/29/30 in shorter months).
//
// It exists so the seeded ledger can span several months. The dashboard's
// cash-flow chart is a trailing six-month window: a seed that only ever posts
// inside the current month leaves five of the six buckets permanently at zero,
// which reads as a broken chart rather than a quiet business.
func monthDay(months, day int) string {
	month := time.Now().AddDate(0, -months, 0)
	last := month.AddDate(0, 1, -1).Day()
	if day > last {
		day = last
	}
	if day < 1 {
		day = 1
	}
	return time.Date(month.Year(), month.Month(), day, 0, 0, 0, 0, month.Location()).Format("2006-01-02")
}

// coaSeed describes one chart-of-accounts row. OpeningBalance is the position
// carried into the demo period, before any seeded journal is applied.
//
// The opening position must itself satisfy Assets = Liabilities + Equity,
// because the journals layered on top are individually balanced and therefore
// cannot correct an unbalanced opening. The invariant is asserted by
// TestSeedOpeningBalancesBalance.
type coaSeed struct {
	code           string
	name           string
	typ            string
	parent         string
	isSystem       bool
	openingBalance int64
}

// demoCOA returns the chart of accounts. Account codes follow the Indonesian
// PSAK convention: 1xxx assets, 2xxx liabilities, 3xxx equity, 4xxx revenue,
// 5xxx operating expenses, 6xxx cost of goods sold.
func demoCOA() []coaSeed {
	return []coaSeed{
		// Group headers (parent rows carry no balance of their own).
		{code: "1000", name: "ASSETS", typ: domain.TypeAsset, isSystem: true},
		{code: "2000", name: "LIABILITIES", typ: domain.TypeLiability, isSystem: true},
		{code: "3000", name: "EQUITY", typ: domain.TypeEquity, isSystem: true},
		{code: "4000", name: "REVENUE", typ: domain.TypeRevenue, isSystem: true},
		{code: "5000", name: "EXPENSES", typ: domain.TypeExpense, isSystem: true},
		{code: "6000", name: "COST OF GOODS SOLD", typ: domain.TypeExpense, isSystem: true},

		// Assets
		{code: "1100", name: "Cash & Cash Equivalents", typ: domain.TypeAsset, parent: "1000", isSystem: true, openingBalance: 62_500_000},
		{code: "1110", name: "Operating Bank Account", typ: domain.TypeAsset, parent: "1000", openingBalance: 248_300_000},
		{code: "1120", name: "Payroll Bank Account", typ: domain.TypeAsset, parent: "1000", openingBalance: 74_100_000},
		{code: "1200", name: "Accounts Receivable", typ: domain.TypeAsset, parent: "1000", isSystem: true, openingBalance: 41_800_000},
		{code: "1300", name: "Merchandise Inventory", typ: domain.TypeAsset, parent: "1000", openingBalance: 96_400_000},
		{code: "1400", name: "Prepaid Rent", typ: domain.TypeAsset, parent: "1000", openingBalance: 18_000_000},
		{code: "1600", name: "Fixed Assets - Vehicles", typ: domain.TypeAsset, parent: "1000", openingBalance: 310_000_000},
		{code: "1610", name: "Fixed Assets - Office Equipment", typ: domain.TypeAsset, parent: "1000", openingBalance: 88_500_000},
		{code: "1690", name: "Accumulated Depreciation", typ: domain.TypeAsset, parent: "1000", isSystem: true, openingBalance: -96_000_000},

		// Liabilities
		{code: "2100", name: "Accounts Payable", typ: domain.TypeLiability, parent: "2000", isSystem: true, openingBalance: 58_400_000},
		{code: "2200", name: "VAT Payable", typ: domain.TypeLiability, parent: "2000", isSystem: true, openingBalance: 7_250_000},
		{code: "2210", name: "Income Tax Art 21 Payable", typ: domain.TypeLiability, parent: "2000", isSystem: true, openingBalance: 2_100_000},
		{code: "2300", name: "Short-Term Bank Loan", typ: domain.TypeLiability, parent: "2000", openingBalance: 150_000_000},

		// Equity
		{code: "3100", name: "Paid-In Capital", typ: domain.TypeEquity, parent: "3000", openingBalance: 425_000_000},
		{code: "3200", name: "Retained Earnings", typ: domain.TypeEquity, parent: "3000", isSystem: true, openingBalance: 200_850_000},

		// Revenue
		{code: "4100", name: "Product Sales", typ: domain.TypeRevenue, parent: "4000"},
		{code: "4200", name: "Service Revenue", typ: domain.TypeRevenue, parent: "4000"},
		{code: "4900", name: "Sales Discounts", typ: domain.TypeRevenue, parent: "4000"},

		// Operating expenses
		{code: "5100", name: "Salaries & Wages Expense", typ: domain.TypeExpense, parent: "5000"},
		{code: "5200", name: "Rent Expense", typ: domain.TypeExpense, parent: "5000"},
		{code: "5300", name: "Utilities Expense", typ: domain.TypeExpense, parent: "5000"},
		{code: "5400", name: "Depreciation Expense", typ: domain.TypeExpense, parent: "5000", isSystem: true},
		{code: "5500", name: "Marketing Expense", typ: domain.TypeExpense, parent: "5000"},
		{code: "5900", name: "Other Operating Expenses", typ: domain.TypeExpense, parent: "5000"},

		// Cost of goods sold
		{code: "6100", name: "Cost of Goods Sold", typ: domain.TypeExpense, parent: "6000"},
	}
}

func demoOpeningLines(accountIDs map[string]uint) []domain.JournalLine {
	lines := make([]domain.JournalLine, 0)
	for _, row := range demoCOA() {
		if row.openingBalance == 0 {
			continue
		}
		line := domain.JournalLine{AccountID: accountIDs[row.code], Memo: "Opening balance"}
		amount := row.openingBalance
		if amount < 0 {
			amount = -amount
		}
		debitNormal := row.typ == domain.TypeAsset || row.typ == domain.TypeExpense
		if (row.openingBalance > 0 && debitNormal) || (row.openingBalance < 0 && !debitNormal) {
			line.Debit = amount
		} else {
			line.Credit = amount
		}
		lines = append(lines, line)
	}
	return lines
}

// seededInvoice describes a demo sales invoice.
type seededInvoice struct {
	number      string
	customer    string
	daysAgo     int
	termDays    int
	status      string
	subtotal    int64
	taxAmount   int64
	branchIndex int
}

func (inv seededInvoice) issueDate() string { return dateOffset(-inv.daysAgo) }
func (inv seededInvoice) dueDate() string   { return dateOffset(-inv.daysAgo + inv.termDays) }
func (inv seededInvoice) total() int64      { return inv.subtotal + inv.taxAmount }

// buildDemoInvoices returns the invoice set. Statuses are chosen so the AR
// aging report has content in every bucket, including more than 90 days.
func buildDemoInvoices() []seededInvoice {
	return []seededInvoice{
		// Settled, comfortably inside terms historically.
		{number: "INV-0241", customer: "PT Digital Sejahtera", daysAgo: 62, termDays: 30, status: domain.InvoiceStatusPaid, subtotal: 41_250_000, taxAmount: 4_537_500, branchIndex: 0},
		{number: "INV-0240", customer: "Toko Berkah Utama", daysAgo: 55, termDays: 14, status: domain.InvoiceStatusPaid, subtotal: 6_800_000, taxAmount: 748_000, branchIndex: 0},
		// Overdue — populates the 1-30 / 31-60 / 61-90 / >90 day buckets.
		{number: "INV-0239", customer: "CV Maju Terus", daysAgo: 58, termDays: 30, status: domain.InvoiceStatusOverdue, subtotal: 18_600_000, taxAmount: 2_046_000, branchIndex: 0},
		{number: "INV-0238", customer: "PT Sumber Makmur", daysAgo: 76, termDays: 30, status: domain.InvoiceStatusOverdue, subtotal: 9_450_000, taxAmount: 1_039_500, branchIndex: 1},
		{number: "INV-0237", customer: "CV Sinar Abadi", daysAgo: 104, termDays: 45, status: domain.InvoiceStatusOverdue, subtotal: 13_200_000, taxAmount: 1_452_000, branchIndex: 0},
		{number: "INV-0236", customer: "Lighthouse Agency", daysAgo: 46, termDays: 7, status: domain.InvoiceStatusOverdue, subtotal: 4_750_000, taxAmount: 522_500, branchIndex: 2},
		// Sent, still within terms.
		{number: "INV-0235", customer: "Lighthouse Agency", daysAgo: 12, termDays: 30, status: domain.InvoiceStatusIssued, subtotal: 27_500_000, taxAmount: 3_025_000, branchIndex: 0},
		{number: "INV-0234", customer: "PT Digital Sejahtera", daysAgo: 8, termDays: 30, status: domain.InvoiceStatusIssued, subtotal: 15_750_000, taxAmount: 1_732_500, branchIndex: 2},
		{number: "INV-0233", customer: "Toko Berkah Utama", daysAgo: 4, termDays: 14, status: domain.InvoiceStatusIssued, subtotal: 5_400_000, taxAmount: 594_000, branchIndex: 1},
		// Draft — excluded from receivables until issued.
		{number: "INV-0232", customer: "CV Sinar Abadi", daysAgo: 2, termDays: 30, status: domain.InvoiceStatusDraft, subtotal: 8_950_000, taxAmount: 984_500, branchIndex: 0},
		{number: "INV-0231", customer: "PT Sumber Makmur", daysAgo: 1, termDays: 30, status: domain.InvoiceStatusDraft, subtotal: 3_100_000, taxAmount: 341_000, branchIndex: 0},
	}
}

// demoCustomers returns the customer roster used by the demo tenant.
func demoCustomers() []domain.Customer {
	seeds := []struct {
		name    string
		email   string
		phone   string
		address string
	}{
		{"PT Sumber Makmur", "finance@sumbermakmur.co.id", "021-5550101", "Kawasan Industri Blok B, Jakarta"},
		{"CV Sinar Abadi", "ar@sinarabadi.co.id", "021-5550102", "Jl. Raya Bekasi KM 18, Bekasi"},
		{"PT Digital Sejahtera", "billing@digitalsejahtera.id", "021-5550103", "Menara Sudirman Lt. 12, Jakarta"},
		{"CV Maju Terus", "keuangan@majuterus.co.id", "022-5550104", "Jl. Soekarno Hatta 220, Bandung"},
		{"Toko Berkah Utama", "berkahutama@gmail.com", "031-5550105", "Pasar Turi Blok C, Surabaya"},
		{"Lighthouse Agency", "invoice@lighthouse.agency", "021-5550106", "Jl. Kemang Selatan 8, Jakarta"},
	}
	out := make([]domain.Customer, 0, len(seeds))
	for _, s := range seeds {
		out = append(out, domain.Customer{Name: s.name, Email: s.email, Phone: s.phone, Address: s.address})
	}
	return out
}

// journalLineSeed is one debit or credit line, referencing an account by code.
type journalLineSeed struct {
	accountCode string
	debit       int64
	credit      int64
	memo        string
}

// journalSeed describes one balanced journal entry. Lines reference accounts
// by code so the seed stays readable; accounts are resolved after the COA is
// created.
//
// A seed carries either daysAgo (an offset from today, for the busy current
// period) or an explicit date (for the historical months that give the
// dashboard's six-month chart something to plot). When both are set, date wins.
type journalSeed struct {
	number  string
	date    string
	daysAgo int
	source  string
	status  string
	memo    string
	lines   []journalLineSeed
}

// entryDate resolves the seed's posting date.
func (s journalSeed) entryDate() string {
	if s.date != "" {
		return s.date
	}
	return dateOffset(-s.daysAgo)
}

// demoJournals returns a realistic, internally balanced set of entries for the
// demo tenant. Every entry satisfies sum(debit) == sum(credit), so applying
// them to the opening balances keeps the books in balance.
func demoJournals() []journalSeed {
	return []journalSeed{
		// --- Posted entries: drive every report ---

		{number: "JRN-1063", daysAgo: 0, source: domain.SourceInvoice, status: domain.JournalStatusPosted,
			memo: "Product Sales - PT Sumber Makmur",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 3_441_000, memo: "Receivable recognized"},
				{accountCode: "4100", credit: 3_100_000, memo: "Product sales revenue"},
				{accountCode: "2200", credit: 341_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1062", daysAgo: 1, source: domain.SourceInvoice, status: domain.JournalStatusPosted,
			memo: "Service Revenue - Lighthouse Agency",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 30_525_000, memo: "Receivable recognized"},
				{accountCode: "4200", credit: 27_500_000, memo: "Consulting service revenue"},
				{accountCode: "2200", credit: 3_025_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1061", daysAgo: 2, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Office Utilities Bill Payment",
			lines: []journalLineSeed{
				{accountCode: "5300", debit: 4_200_000, memo: "Monthly utilities expense"},
				{accountCode: "1110", credit: 4_200_000, memo: "Bank transfer - Operating BCA"},
			}},

		{number: "JRN-1060", daysAgo: 4, source: domain.SourceInvoice, status: domain.JournalStatusPosted,
			memo: "Product Sales - PT Digital Sejahtera",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 17_482_500, memo: "Receivable recognized"},
				{accountCode: "4100", credit: 15_750_000, memo: "Product sales revenue"},
				{accountCode: "2200", credit: 1_732_500, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1059", daysAgo: 6, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Customer Payment Received INV-0240",
			lines: []journalLineSeed{
				{accountCode: "1110", debit: 7_548_000, memo: "Cash receipt - BCA"},
				{accountCode: "1200", credit: 7_548_000, memo: "Receivable settled"},
			}},

		{number: "JRN-1058", daysAgo: 8, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Inventory Purchase - CV Sinar Abadi",
			lines: []journalLineSeed{
				{accountCode: "1300", debit: 22_000_000, memo: "Inventory received into warehouse"},
				{accountCode: "2100", credit: 22_000_000, memo: "Accounts payable to vendor"},
			}},

		{number: "JRN-1057", daysAgo: 11, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Monthly Employee Payroll Payment",
			lines: []journalLineSeed{
				{accountCode: "5100", debit: 32_000_000, memo: "Gross salary expense"},
				{accountCode: "2210", credit: 3_850_000, memo: "Income tax Art 21 withheld"},
				{accountCode: "1120", credit: 28_150_000, memo: "Bank transfer - Payroll Mandiri"},
			}},

		{number: "JRN-1056", daysAgo: 14, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Monthly Fixed Asset Depreciation",
			lines: []journalLineSeed{
				{accountCode: "5400", debit: 7_700_000, memo: "Depreciation expense"},
				{accountCode: "1690", credit: 7_700_000, memo: "Accumulated depreciation"},
			}},

		{number: "JRN-1055", daysAgo: 17, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Main Office Rent Payment",
			lines: []journalLineSeed{
				{accountCode: "5200", debit: 12_000_000, memo: "Monthly rent expense"},
				{accountCode: "1110", credit: 12_000_000, memo: "Bank transfer - Operating BCA"},
			}},

		{number: "JRN-1054", daysAgo: 20, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Cost of Goods Sold on Product Sales",
			lines: []journalLineSeed{
				{accountCode: "6100", debit: 11_400_000, memo: "COGS for sold products"},
				{accountCode: "1300", credit: 11_400_000, memo: "Inventory reduction"},
			}},

		{number: "JRN-1053", daysAgo: 23, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Short-Term Bank Loan Installment Payment",
			lines: []journalLineSeed{
				{accountCode: "2300", debit: 10_000_000, memo: "Principal repayment"},
				{accountCode: "1110", credit: 10_000_000, memo: "Bank transfer - Operating BCA"},
			}},

		{number: "JRN-1052", daysAgo: 26, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Digital Marketing & Trade Exhibition Expense",
			lines: []journalLineSeed{
				{accountCode: "5500", debit: 6_300_000, memo: "Marketing expense"},
				{accountCode: "1110", credit: 6_300_000, memo: "Bank transfer - Operating BCA"},
			}},

		{number: "JRN-1064", daysAgo: 3, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Daily Cash Sales - Toko Berkah Utama",
			lines: []journalLineSeed{
				{accountCode: "1120", debit: 5_994_000, memo: "Cash received - Payroll Mandiri"},
				{accountCode: "4100", credit: 5_400_000, memo: "Cash product sales"},
				{accountCode: "2200", credit: 594_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1065", daysAgo: 5, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Product Sales - CV Sinar Abadi (INV-0232)",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 9_934_500, memo: "Receivable recognized"},
				{accountCode: "4100", credit: 8_950_000, memo: "Product sales revenue"},
				{accountCode: "2200", credit: 984_500, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1066", daysAgo: 7, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Cost of Goods Sold on Cash & Credit Sales",
			lines: []journalLineSeed{
				{accountCode: "6100", debit: 8_600_000, memo: "COGS for sold products"},
				{accountCode: "1300", credit: 8_600_000, memo: "Inventory reduction"},
			}},

		{number: "JRN-1067", daysAgo: 10, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Maintenance Service Revenue - PT Sumber Makmur",
			lines: []journalLineSeed{
				{accountCode: "1110", debit: 12_210_000, memo: "Cash receipt - BCA"},
				{accountCode: "4200", credit: 11_000_000, memo: "Service revenue"},
				{accountCode: "2200", credit: 1_210_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1068", daysAgo: 9, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Product Sales - PT Digital Sejahtera (INV-0236)",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 30_525_000, memo: "Receivable recognized"},
				{accountCode: "4100", credit: 27_500_000, memo: "Product sales revenue"},
				{accountCode: "2200", credit: 3_025_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1069", daysAgo: 12, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Product Sales - CV Maju Terus (INV-0239)",
			lines: []journalLineSeed{
				{accountCode: "1200", debit: 20_646_000, memo: "Receivable recognized"},
				{accountCode: "4100", credit: 18_600_000, memo: "Product sales revenue"},
				{accountCode: "2200", credit: 2_046_000, memo: "Output VAT 11%"},
			}},

		{number: "JRN-1070", daysAgo: 13, source: domain.SourceManual, status: domain.JournalStatusPosted,
			memo: "Cost of Goods Sold on Wholesale Orders",
			lines: []journalLineSeed{
				{accountCode: "6100", debit: 34_200_000, memo: "COGS for sold products"},
				{accountCode: "1300", credit: 34_200_000, memo: "Inventory reduction"},
			}},

		// --- Draft entries: must never appear in posted reports ---

		{number: "JRN-1051", daysAgo: 5, source: domain.SourceManual, status: domain.JournalStatusDraft,
			memo: "Employee Bonus Accrual (pending approval)",
			lines: []journalLineSeed{
				{accountCode: "5100", debit: 5_000_000, memo: "Bonus expense accrual"},
				{accountCode: "2100", credit: 5_000_000, memo: "Employee bonus payable"},
			}},

		{number: "JRN-1050", daysAgo: 9, source: domain.SourceManual, status: domain.JournalStatusDraft,
			memo: "Damaged Inventory Adjustment (pending verification)",
			lines: []journalLineSeed{
				{accountCode: "6100", debit: 850_000, memo: "Inventory shrinkage expense"},
				{accountCode: "1300", credit: 850_000, memo: "Damaged inventory write-off"},
			}},
	}
}

// historicalMonths is how many trailing months demoHistoryJournals() covers.
// It matches the dashboard's six-month chart window (the current month plus
// these five), so every bucket has real activity behind it.
const historicalMonths = 5

// demoHistoryJournals returns a trailing run of balanced monthly entries.
//
// Why this exists: the dashboard plots six months of cash movement, but the
// hand-written journals in demoJournals() all sit inside the last ~27 days.
// A chart with five permanently empty buckets reads as broken, not as a quiet
// business. These entries are generated per month instead, with amounts that
// vary slightly month to month so the series looks like a trading trend rather
// than a copy-pasted block.
//
// Accounting integrity is preserved by construction, not by coincidence:
//
//   - Every entry balances (debits == credits), so the opening position stays
//     valid and Assets = Liabilities + Equity keeps holding. The invariant is
//     still asserted by TestSeed_OpeningBalancesSatisfyAccountingEquation.
//   - Revenue is credited against cash or receivables, never against nothing.
//   - COGS is debited and inventory credited, so the inventory account stays
//     positive instead of draining into a negative balance.
//   - Output VAT (11%) is credited alongside revenue, matching the current-period
//     entries and keeping the tax report internally consistent.
func demoHistoryJournals() []journalSeed {
	out := make([]journalSeed, 0, historicalMonths*11)

	for m := historicalMonths; m >= 1; m-- {
		// A gentle month-to-month variation, so the six-point series reads as a
		// trend. Fixed, deterministic values keep the seeded books reproducible.
		growth := 1 + float64(historicalMonths-m)*0.04

		// Revenue must comfortably clear COGS + operating expenses, otherwise the
		// trailing months drag the period P&L into a loss and the demo tenant
		// reads as a business going under. These figures leave roughly a 30%
		// gross margin, which is plausible for a trading business.
		sales := int64(float64(105_000_000) * growth)
		service := int64(float64(38_000_000) * growth)
		cogs := int64(float64(58_000_000) * growth)
		rent := int64(float64(12_000_000) * growth)
		utilities := 4_100_000 + int64(m)*90_000
		payroll := 29_500_000 + int64(m)*150_000
		marketing := 3_200_000 + int64(m)*120_000
		var depreciation int64 = 7_700_000

		// Merchandise bought on credit. Without this the monthly COGS entry
		// would credit inventory five times over and drive the account negative,
		// which no real balance sheet would carry.
		purchases := int64(float64(64_000_000) * growth)
		// The supplier is paid the following month. Only part of the purchase is
		// settled in cash, with the rest left on account: paying the whole amount
		// every month drains the operating bank faster than the sales that fund it
		// arrive, and the balance sheet ends up carrying a negative bank balance.
		supplierPaid := purchases / 2

		// VAT is 11% of the taxable amount; rounded to the nearest rupiah so the
		// tax report never shows a fractional line.
		salesVAT := (sales*11 + 50) / 100
		serviceVAT := (service*11 + 50) / 100
		payrollTax := (payroll*12 + 50) / 100
		// Output VAT collected is remitted the month after it is charged.
		vatPaid := (sales*11 + 50) / 100

		// Most takings are banked rather than held in the till, so the operating
		// bank absorbs the payroll and supplier runs and stays comfortably
		// positive. Routing everything through cash on hand instead leaves the
		// payroll account overdrawn and the cash balance implausibly large.
		cashSales := sales / 5
		bankSales := sales - cashSales
		cashVAT := salesVAT / 5
		bankVAT := salesVAT - cashVAT
		// Net salary is what leaves the company, so the payroll account is funded
		// by an internal transfer before it is paid out. Without that transfer the
		// account drains below zero over the seeded months.
		netPayroll := payroll - payrollTax

		out = append(out,
			// Cash-and-carry product sales: money in the door the same day. Only the
			// till portion of the month's product sales is recognised here; the rest
			// is banked in the entry below, so the two must sum back to `sales`.
			journalSeed{
				number: historyNumber(m, 1), date: monthDay(m, 6),
				source: domain.SourceInvoice, status: domain.JournalStatusPosted,
				memo: "Product Sales - Retail Counter (closing)",
				lines: []journalLineSeed{
					{accountCode: "1100", debit: cashSales + cashVAT, memo: "Cash sales received"},
					{accountCode: "4100", credit: cashSales, memo: "Product sales revenue"},
					{accountCode: "2200", credit: cashVAT, memo: "Output VAT 11%"},
				},
			},
			// The bulk of the month's takings, settled by bank transfer.
			journalSeed{
				number: historyNumber(m, 10), date: monthDay(m, 8),
				source: domain.SourceInvoice, status: domain.JournalStatusPosted,
				memo: "Product Sales - Corporate Accounts",
				lines: []journalLineSeed{
					{accountCode: "1110", debit: bankSales + bankVAT, memo: "Bank transfer - BCA"},
					{accountCode: "4100", credit: bankSales, memo: "Product sales revenue"},
					{accountCode: "2200", credit: bankVAT, memo: "Output VAT 11%"},
				},
			},
			// Service work billed and collected.
			journalSeed{
				number: historyNumber(m, 2), date: monthDay(m, 14),
				source: domain.SourceInvoice, status: domain.JournalStatusPosted,
				memo: "Service Revenue - Contracting Works",
				lines: []journalLineSeed{
					{accountCode: "1110", debit: service + serviceVAT, memo: "Bank transfer - BCA"},
					{accountCode: "4200", credit: service, memo: "Service revenue"},
					{accountCode: "2200", credit: serviceVAT, memo: "Output VAT 11%"},
				},
			},
			// Cost of the goods sold above, so the margin stays plausible.
			journalSeed{
				number: historyNumber(m, 3), date: monthDay(m, 21),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Cost of Goods Sold - Monthly Closing",
				lines: []journalLineSeed{
					{accountCode: "6100", debit: cogs, memo: "COGS for sold products"},
					{accountCode: "1300", credit: cogs, memo: "Inventory reduction"},
				},
			},
			// Operating costs, all settled in cash: this is the outflow side
			// that makes each month a two-sided bar on the chart.
			journalSeed{
				number: historyNumber(m, 4), date: monthDay(m, 26),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Operating Expenses - Rent, Utilities & Marketing",
				lines: []journalLineSeed{
					{accountCode: "5200", debit: rent, memo: "Rent expense"},
					{accountCode: "5300", debit: utilities, memo: "Utilities expense"},
					{accountCode: "5500", debit: marketing, memo: "Marketing expense"},
					{accountCode: "1110", credit: rent + utilities + marketing, memo: "Bank transfer - Operating BCA"},
				},
			},
			// Fund the dedicated payroll account from the operating bank before
			// payroll runs. This is an internal transfer, so it nets to zero across
			// the group and never distorts a report - but it is what keeps the
			// payroll account from going overdrawn over the seeded months.
			journalSeed{
				number: historyNumber(m, 11), date: monthDay(m, 26),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Internal Transfer - Fund Payroll Account",
				lines: []journalLineSeed{
					{accountCode: "1120", debit: netPayroll, memo: "Transfer in from operating account"},
					{accountCode: "1110", credit: netPayroll, memo: "Transfer out to payroll account"},
				},
			},
			journalSeed{
				number: historyNumber(m, 5), date: monthDay(m, 27),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Monthly Employee Payroll Payment",
				lines: []journalLineSeed{
					{accountCode: "5100", debit: payroll, memo: "Gross salary expense"},
					{accountCode: "2210", credit: payrollTax, memo: "Income tax Art 21 withheld"},
					{accountCode: "1120", credit: netPayroll, memo: "Bank transfer - Payroll Mandiri"},
				},
			},
			journalSeed{
				number: historyNumber(m, 6), date: monthDay(m, 28),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Monthly Fixed Asset Depreciation",
				lines: []journalLineSeed{
					{accountCode: "5400", debit: depreciation, memo: "Depreciation expense"},
					{accountCode: "1690", credit: depreciation, memo: "Accumulated depreciation"},
				},
			},
			// Output VAT charged during the month is remitted to the tax office.
			journalSeed{
				number: historyNumber(m, 7), date: monthDay(m, 28),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "VAT Remittance to the Tax Office",
				lines: []journalLineSeed{
					{accountCode: "2200", debit: vatPaid, memo: "Output VAT paid"},
					{accountCode: "1110", credit: vatPaid, memo: "Bank transfer - Bank Mandiri"},
				},
			},
			// Merchandise received on credit. Non-cash, so it does not touch the
			// cash accounts, but it keeps the inventory account funded against the
			// COGS entry above.
			journalSeed{
				number: historyNumber(m, 8), date: monthDay(m, 18),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Inventory Purchase - Distributor Terms",
				lines: []journalLineSeed{
					{accountCode: "1300", debit: purchases, memo: "Goods received into warehouse"},
					{accountCode: "2100", credit: purchases, memo: "Accounts payable to supplier"},
				},
			},
			// Settling the prior month's purchases: the second cash outflow.
			journalSeed{
				number: historyNumber(m, 9), date: monthDay(m, 24),
				source: domain.SourceManual, status: domain.JournalStatusPosted,
				memo: "Supplier Payment - Inventory Settlement",
				lines: []journalLineSeed{
					{accountCode: "2100", debit: supplierPaid, memo: "Accounts payable settled"},
					{accountCode: "1110", credit: supplierPaid, memo: "Bank transfer - Operating BCA"},
				},
			},
		)
	}

	return out
}

// historyNumber builds a stable, sortable document number for a generated
// historical entry: JRN-08MSS, where M is the month distance counted from the
// OLDEST generated month and SS is the entry's sequence within that month.
//
// The month digit is inverted on purpose (historicalMonths - monthsBack + 1) so
// that string-descending order matches chronological order, which is how both
// stores sort journals. The current-period JRN-10xx block still sorts above the
// whole JRN-08xx history range.
func historyNumber(monthsBack, seq int) string {
	monthRank := historicalMonths - monthsBack + 1
	return fmt.Sprintf("JRN-08%d%02d", monthRank, seq)
}
