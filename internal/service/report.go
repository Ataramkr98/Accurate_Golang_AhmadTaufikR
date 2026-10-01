package service

import (
	"fmt"
	"sort"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

// ReportService aggregates account balances into financial statements.
type ReportService struct {
	store repository.Store
}

func NewReportService(store repository.Store) *ReportService {
	return &ReportService{store: store}
}

// LedgerRow is a running-balance row in the general ledger.
type LedgerRow struct {
	Date    string `json:"date"`
	Ref     string `json:"ref"`
	Memo    string `json:"memo"`
	Debit   int64  `json:"debit"`
	Credit  int64  `json:"credit"`
	Balance int64  `json:"balance"`
}

// LedgerDetail is the full general-ledger payload for one account.
type LedgerDetail struct {
	AccountID      uint        `json:"accountId"`
	AccountCode    string      `json:"accountCode"`
	AccountName    string      `json:"accountName"`
	Opening        int64       `json:"openingBalance"`
	Rows           []LedgerRow `json:"rows"`
	TotalDebit     int64       `json:"totalDebit"`
	TotalCredit    int64       `json:"totalCredit"`
	ClosingBalance int64       `json:"closingBalance"`
}

// Ledger returns a running-balance ledger for one account.
// The opening balance is the current balance minus the sum of posted
// movements so far (MVP: no period filtering).
func (s *ReportService) Ledger(companyID, accountID uint) (*LedgerDetail, error) {
	account, err := s.store.GetAccount(companyID, accountID)
	if err != nil {
		return nil, err
	}
	entries := s.store.ListLedgerEntries(companyID, accountID)

	detail := &LedgerDetail{
		AccountID: accountID, AccountCode: account.Code, AccountName: account.Name,
		Rows: make([]LedgerRow, 0, len(entries)),
	}

	var movement int64
	for _, e := range entries {
		movement += balanceDelta(account.Type, e.Debit, e.Credit)
		detail.TotalDebit += e.Debit
		detail.TotalCredit += e.Credit
	}

	opening := account.Balance - movement
	detail.Opening = opening

	balance := opening
	for _, e := range entries {
		balance += balanceDelta(account.Type, e.Debit, e.Credit)
		detail.Rows = append(detail.Rows, LedgerRow{
			Date: e.Date, Ref: e.Ref, Memo: e.Memo,
			Debit: e.Debit, Credit: e.Credit, Balance: balance,
		})
	}
	detail.ClosingBalance = balance
	return detail, nil
}

// ReportAccount is one line in a balance-sheet / P&L statement.
type ReportAccount struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Balance int64  `json:"balance"`
}

// BalanceSheetSection groups asset/liability/equity accounts.
type BalanceSheetSection struct {
	Label    string          `json:"label"`
	Accounts []ReportAccount `json:"accounts"`
	Total    int64           `json:"total"`
}

// BalanceSheet is the position statement.
type BalanceSheet struct {
	CompanyName         string                `json:"companyName"`
	SAKMode             string                `json:"sakMode"`
	Assets              []BalanceSheetSection `json:"assets"`
	Liabilities         []BalanceSheetSection `json:"liabilities"`
	Equity              []BalanceSheetSection `json:"equity"`
	TotalAssets         int64                 `json:"totalAssets"`
	TotalLiabilities    int64                 `json:"totalLiabilities"`
	TotalEquity         int64                 `json:"totalEquity"`
	CurrentPeriodProfit int64                 `json:"currentPeriodProfit"`
}

// BalanceSheet groups accounts by type and computes totals.
func (s *ReportService) BalanceSheet(companyID uint) (*BalanceSheet, error) {
	return s.BalanceSheetScoped(companyID, 0, "")
}

// BalanceSheetScoped returns the position through an inclusive date for one
// branch, or all branches when branchID is zero.
func (s *ReportService) BalanceSheetScoped(companyID, branchID uint, asOf string) (*BalanceSheet, error) {
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	accounts := s.scopedAccounts(companyID, branchID, "", asOf)

	bs := &BalanceSheet{CompanyName: company.Name, SAKMode: company.SAKMode}

	groupAssets := map[string]*BalanceSheetSection{}
	groupLiabs := map[string]*BalanceSheetSection{}
	groupEq := map[string]*BalanceSheetSection{}

	for _, a := range accounts {
		if a.ParentID == 0 {
			continue // skip root group headers
		}
		item := ReportAccount{Code: a.Code, Name: a.Name, Balance: a.Balance}
		switch a.Type {
		case domain.TypeAsset:
			key := assetGroup(a.Code)
			section, ok := groupAssets[key]
			if !ok {
				section = &BalanceSheetSection{Label: key, Accounts: []ReportAccount{}}
				groupAssets[key] = section
			}
			section.Accounts = append(section.Accounts, item)
			section.Total += a.Balance
			bs.TotalAssets += a.Balance
		case domain.TypeLiability:
			section, ok := groupLiabs["Liabilities"]
			if !ok {
				section = &BalanceSheetSection{Label: "Liabilities", Accounts: []ReportAccount{}}
				groupLiabs["Liabilities"] = section
			}
			section.Accounts = append(section.Accounts, item)
			section.Total += a.Balance
			bs.TotalLiabilities += a.Balance
		case domain.TypeEquity:
			section, ok := groupEq["Equity"]
			if !ok {
				section = &BalanceSheetSection{Label: "Equity", Accounts: []ReportAccount{}}
				groupEq["Equity"] = section
			}
			section.Accounts = append(section.Accounts, item)
			section.Total += a.Balance
			bs.TotalEquity += a.Balance
		}
	}

	// Current-period profit = sum of revenue minus expenses (running balances).
	var profit int64
	for _, a := range accounts {
		switch a.Type {
		case domain.TypeRevenue:
			if a.ParentID != 0 {
				profit += a.Balance
			}
		case domain.TypeExpense:
			if a.ParentID != 0 {
				profit -= a.Balance
			}
		}
	}
	bs.CurrentPeriodProfit = profit

	// Attach current-period profit as part of equity (running earnings). The
	// profit is presented as a section line but TotalEquity must not be
	// incremented again — it is already reflected in the equity accounts.
	if profit != 0 {
		eq := groupEq["Equity"]
		if eq == nil {
			eq = &BalanceSheetSection{Label: "Equity", Accounts: []ReportAccount{}}
			groupEq["Equity"] = eq
		}
		eq.Accounts = append(eq.Accounts, ReportAccount{Code: "3999", Name: "Current period profit", Balance: profit})
		eq.Total += profit
	}

	bs.Assets = sortedSections(groupAssets)
	bs.Liabilities = sortedSections(groupLiabs)
	bs.Equity = sortedSections(groupEq)
	return bs, nil
}

// assetGroup buckets asset accounts into the classic current/fixed split.
func assetGroup(code string) string {
	if code >= "1600" {
		return "Fixed assets"
	}
	return "Current assets"
}

func sortedSections(m map[string]*BalanceSheetSection) []BalanceSheetSection {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]BalanceSheetSection, 0, len(keys))
	for _, k := range keys {
		out = append(out, *m[k])
	}
	return out
}

// ProfitLossSection groups revenue/expense accounts.
type ProfitLossSection struct {
	Label    string          `json:"label"`
	Accounts []ReportAccount `json:"accounts"`
	Total    int64           `json:"total"`
}

// ProfitLoss is the income statement.
type ProfitLoss struct {
	CompanyName      string            `json:"companyName"`
	SAKMode          string            `json:"sakMode"`
	Revenue          ProfitLossSection `json:"revenue"`
	COGS             ProfitLossSection `json:"cogs"`
	GrossProfit      int64             `json:"grossProfit"`
	OperatingExpense ProfitLossSection `json:"operatingExpense"`
	NetIncome        int64             `json:"netIncome"`
}

// ProfitLoss groups revenue and expense accounts.
func (s *ReportService) ProfitLoss(companyID uint) (*ProfitLoss, error) {
	return s.ProfitLossScoped(companyID, 0, "", "")
}

// ProfitLossScoped includes posted activity in the inclusive date range.
func (s *ReportService) ProfitLossScoped(companyID, branchID uint, from, to string) (*ProfitLoss, error) {
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	accounts := s.scopedAccounts(companyID, branchID, from, to)

	pl := &ProfitLoss{
		CompanyName:      company.Name,
		SAKMode:          company.SAKMode,
		Revenue:          ProfitLossSection{Label: "Revenue", Accounts: []ReportAccount{}},
		COGS:             ProfitLossSection{Label: "Cost of sales", Accounts: []ReportAccount{}},
		OperatingExpense: ProfitLossSection{Label: "Operating expenses", Accounts: []ReportAccount{}},
	}

	for _, a := range accounts {
		if a.ParentID == 0 {
			continue
		}
		item := ReportAccount{Code: a.Code, Name: a.Name, Balance: a.Balance}
		switch a.Type {
		case domain.TypeRevenue:
			pl.Revenue.Accounts = append(pl.Revenue.Accounts, item)
			pl.Revenue.Total += a.Balance
		case domain.TypeExpense:
			if a.Code >= "6000" { // COGS bucket
				pl.COGS.Accounts = append(pl.COGS.Accounts, item)
				pl.COGS.Total += a.Balance
			} else {
				pl.OperatingExpense.Accounts = append(pl.OperatingExpense.Accounts, item)
				pl.OperatingExpense.Total += a.Balance
			}
		}
	}

	pl.GrossProfit = pl.Revenue.Total - pl.COGS.Total
	pl.NetIncome = pl.GrossProfit - pl.OperatingExpense.Total
	return pl, nil
}

// DashboardKPI is one headline metric.
type DashboardKPI struct {
	Label    string `json:"label"`
	Value    int64  `json:"value"`
	Delta    string `json:"delta"`
	Positive bool   `json:"positive"`
}

// RecentTransaction is a flattened row for the dashboard's latest activity.
type RecentTransaction struct {
	ID         uint   `json:"id"`
	Number     string `json:"number"`
	Date       string `json:"date"`
	Memo       string `json:"memo"`
	Amount     int64  `json:"amount"`
	Status     string `json:"status"`
	SourceType string `json:"sourceType"`
}

// Dashboard is the summary payload for the main dashboard page.
type Dashboard struct {
	CompanyName        string              `json:"companyName"`
	SAKMode            string              `json:"sakMode"`
	KPIs               []DashboardKPI      `json:"kpis"`
	RecentTransactions []RecentTransaction `json:"recentTransactions"`
	CashFlowSeries     []CashFlowPoint     `json:"cashFlowSeries"`
}

// CashFlowPoint is one month on the dashboard's cash-flow chart. Inflow and
// outflow are the actual debit/credit movements on the cash and bank accounts
// during that month, not an estimate.
type CashFlowPoint struct {
	Period  string `json:"period"` // "YYYY-MM"
	Label   string `json:"label"`  // "Sep 2026"
	Inflow  int64  `json:"inflow"`
	Outflow int64  `json:"outflow"`
	Net     int64  `json:"net"`
}

// cashFlowMonths is how many months the dashboard chart shows.
const cashFlowMonths = 6

// Dashboard aggregates headline KPIs and the latest journal activity.
func (s *ReportService) Dashboard(companyID uint) (*Dashboard, error) {
	return s.DashboardScoped(companyID, 0, "")
}

func (s *ReportService) DashboardScoped(companyID, branchID uint, period string) (*Dashboard, error) {
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	var periodFrom, periodTo string
	if period != "" {
		month, parseErr := time.Parse("2006-01", period)
		if parseErr != nil {
			return nil, domain.ErrValidation("period must use YYYY-MM")
		}
		periodFrom = month.Format(dateLayout)
		periodTo = month.AddDate(0, 1, -1).Format(dateLayout)
	}
	positionAccounts := s.scopedAccounts(companyID, branchID, "", periodTo)
	activityAccounts := s.scopedAccounts(companyID, branchID, periodFrom, periodTo)

	var cash int64
	var profit int64
	cashAccountIDs := map[uint]bool{}
	for _, a := range positionAccounts {
		switch {
		case a.Code == "1100":
			cash += a.Balance
		}
		if isCashAccount(a) {
			cashAccountIDs[a.ID] = true
		}
	}
	for _, a := range activityAccounts {
		if a.Type == domain.TypeRevenue && a.ParentID != 0 {
			profit += a.Balance
		} else if a.Type == domain.TypeExpense && a.ParentID != 0 {
			profit -= a.Balance
		}
	}

	var overdueReceivable int64
	var overdueCount int
	today := time.Now().Format(dateLayout)
	for _, invoice := range s.store.ListInvoices(companyID) {
		if branchID != 0 && invoice.BranchID != branchID {
			continue
		}
		if periodTo != "" && invoice.Date > periodTo {
			continue
		}
		if invoice.Status == domain.InvoiceStatusPaid || invoice.Status == domain.InvoiceStatusDraft {
			continue
		}
		if invoice.Status == domain.InvoiceStatusOverdue || invoice.DueDate < today {
			overdueReceivable += invoice.Total
			overdueCount++
		}
	}

	kpis := []DashboardKPI{
		{Label: "Cash & bank", Value: cash, Delta: "Current balance", Positive: cash >= 0},
		{Label: "Overdue receivables", Value: overdueReceivable, Delta: fmt.Sprintf("%d overdue invoices", overdueCount), Positive: overdueCount == 0},
		{Label: "Current-period profit", Value: profit, Delta: "Revenue less expenses", Positive: profit >= 0},
	}

	// Two different populations, deliberately kept apart:
	//
	//   scoped  - branch filter only. The cash-flow chart is a trailing
	//             six-month window, so it must see every posted entry the
	//             branch ever had, NOT just the selected month's.
	//   entries - branch + selected period. This drives the KPIs and the
	//             "recent journals" list, which are explicitly period-scoped.
	//
	// Passing the period-filtered slice to cashFlowSeries (the earlier
	// behaviour) meant the six-month axis could only ever have one non-empty
	// bucket, so the chart was structurally incapable of showing a trend.
	scoped := filterJournalEntries(s.store.ListJournalEntries(companyID, ""), branchID, "", "")
	entries := filterJournalEntries(scoped, 0, periodFrom, periodTo)

	recent := make([]RecentTransaction, 0, min(6, len(entries)))
	for i := 0; i < len(entries) && i < 6; i++ {
		e := entries[i]
		recent = append(recent, RecentTransaction{
			ID: e.ID, Number: e.Number, Date: e.Date, Memo: e.Memo,
			Amount: entryTotal(e), Status: e.Status, SourceType: e.SourceType,
		})
	}

	return &Dashboard{
		CompanyName: company.Name, SAKMode: company.SAKMode,
		KPIs: kpis, RecentTransactions: recent,
		CashFlowSeries: cashFlowSeries(scoped, cashAccountIDs),
	}, nil
}

// isCashAccount reports whether an account is cash or a bank account. Codes in
// the 1100-1199 band cover cash on hand and bank accounts under PSAK.
func isCashAccount(account domain.Account) bool {
	return account.Type == domain.TypeAsset &&
		len(account.Code) == 4 &&
		account.Code >= "1100" && account.Code <= "1199"
}

// cashFlowSeries buckets posted cash-account movements into monthly inflow and
// outflow totals. Only posted entries count, so drafts never distort the chart.
func cashFlowSeries(entries []domain.JournalEntry, cashAccountIDs map[uint]bool) []CashFlowPoint {
	now := time.Now()
	buckets := make(map[string]*CashFlowPoint, cashFlowMonths)
	order := make([]string, 0, cashFlowMonths)

	// Build the axis first so months with no activity still appear as zero,
	// keeping the chart's x-axis stable.
	for i := cashFlowMonths - 1; i >= 0; i-- {
		month := now.AddDate(0, -i, 0)
		period := month.Format("2006-01")
		buckets[period] = &CashFlowPoint{Period: period, Label: month.Format("Jan 2006")}
		order = append(order, period)
	}

	for _, entry := range entries {
		if entry.Status != domain.JournalStatusPosted {
			continue
		}
		period := entry.Date
		if len(period) >= 7 {
			period = period[:7]
		}
		bucket, ok := buckets[period]
		if !ok {
			continue
		}
		for _, line := range entry.Lines {
			if !cashAccountIDs[line.AccountID] {
				continue
			}
			// Debit increases cash (inflow); credit decreases it (outflow).
			bucket.Inflow += line.Debit
			bucket.Outflow += line.Credit
		}
	}

	series := make([]CashFlowPoint, 0, len(order))
	for _, period := range order {
		point := buckets[period]
		point.Net = point.Inflow - point.Outflow
		series = append(series, *point)
	}
	return series
}

// CashFlow models
type CashFlowItem struct {
	Name   string `json:"name"`
	Amount int64  `json:"amount"`
}

type CashFlowSection struct {
	Title string         `json:"title"`
	Items []CashFlowItem `json:"items"`
	Total int64          `json:"total"`
}

type CashFlowReport struct {
	CompanyName      string          `json:"companyName"`
	SAKMode          string          `json:"sakMode"`
	Operating        CashFlowSection `json:"operating"`
	Investing        CashFlowSection `json:"investing"`
	Financing        CashFlowSection `json:"financing"`
	NetCashChange    int64           `json:"netCashChange"`
	BeginningBalance int64           `json:"beginningBalance"`
	EndingBalance    int64           `json:"endingBalance"`
}

func (s *ReportService) CashFlow(companyID uint) (*CashFlowReport, error) {
	return s.CashFlowScoped(companyID, 0, "", "")
}

func (s *ReportService) CashFlowScoped(companyID, branchID uint, from, to string) (*CashFlowReport, error) {
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}
	accounts := s.scopedAccounts(companyID, branchID, from, to)

	var cashEnding int64
	var revenueTotal int64
	var expenseTotal int64
	var assetCapex int64
	var equityCapital int64

	for _, a := range accounts {
		if a.ParentID == 0 {
			continue
		}
		switch {
		case a.Code == "1100":
			cashEnding += a.Balance
		case a.Type == domain.TypeRevenue:
			revenueTotal += a.Balance
		case a.Type == domain.TypeExpense:
			expenseTotal += a.Balance
		case a.Code >= "1600" && a.Code < "1690":
			assetCapex += a.Balance
		case a.Code == "3100":
			equityCapital += a.Balance
		}
	}

	operatingNet := revenueTotal - expenseTotal
	investingNet := -assetCapex
	financingNet := equityCapital
	netChange := operatingNet + investingNet + financingNet
	begBalance := cashEnding - netChange

	return &CashFlowReport{
		CompanyName: company.Name,
		SAKMode:     company.SAKMode,
		Operating: CashFlowSection{
			Title: "Cash Flow from Operating Activities",
			Items: []CashFlowItem{
				{Name: "Cash Received from Customers", Amount: revenueTotal},
				{Name: "Cash Paid for Operating Expenses & COGS", Amount: -expenseTotal},
			},
			Total: operatingNet,
		},
		Investing: CashFlowSection{
			Title: "Cash Flow from Investing Activities",
			Items: []CashFlowItem{
				{Name: "Acquisition of Fixed Assets & Equipment", Amount: -assetCapex},
			},
			Total: investingNet,
		},
		Financing: CashFlowSection{
			Title: "Cash Flow from Financing Activities",
			Items: []CashFlowItem{
				{Name: "Capital Contributions by Owners", Amount: equityCapital},
			},
			Total: financingNet,
		},
		NetCashChange:    netChange,
		BeginningBalance: begBalance,
		EndingBalance:    cashEnding,
	}, nil
}

type TaxSummaryReport struct {
	PPNKeluaran      int64 `json:"ppnKeluaran"`
	PPNMasukan       int64 `json:"ppnMasukan"`
	PPNTerutang      int64 `json:"ppnTerutang"`
	PPh21Total       int64 `json:"pph21Total"`
	EstimatedPayable int64 `json:"estimatedPayable"`
	TotalTaxInvoices int   `json:"totalTaxInvoices"`
}

// TaxSummary reports the company's standing tax position.
//
// PPN Keluaran is summed from the tax actually charged on issued invoices; the
// input-VAT credit (PPN Masukan) comes from the 2200 account, which holds the
// net VAT liability; and PPh 21 comes from the 2210 withholding account, which
// is credited when payroll is posted. All three are read from posted records —
// nothing here is estimated from a blanket percentage.
func (s *ReportService) TaxSummary(companyID uint) (*TaxSummaryReport, error) {
	return s.TaxSummaryScoped(companyID, 0, "", "")
}

func (s *ReportService) TaxSummaryScoped(companyID, branchID uint, from, to string) (*TaxSummaryReport, error) {
	invoices := s.store.ListInvoices(companyID)
	accounts := s.scopedAccounts(companyID, branchID, from, to)

	var ppnKeluaran int64
	var taxInvoicesCount int
	for _, inv := range invoices {
		if branchID != 0 && inv.BranchID != branchID {
			continue
		}
		if from != "" && inv.Date < from || to != "" && inv.Date > to {
			continue
		}
		if inv.TaxAmount > 0 && inv.Status != domain.InvoiceStatusDraft {
			ppnKeluaran += inv.TaxAmount
			taxInvoicesCount++
		}
	}

	var ppnMasukan int64
	var pph21 int64
	for _, a := range accounts {
		switch a.Code {
		case "2200":
			// Credit-normal liability account: the balance is VAT still owed.
			// A negative balance means input VAT exceeds output VAT (a credit).
			ppnMasukan = a.Balance
		case "2210":
			pph21 = a.Balance
		}
	}

	ppnTerutang := ppnKeluaran - ppnMasukan
	estimatedPayable := ppnTerutang + pph21

	return &TaxSummaryReport{
		PPNKeluaran:      ppnKeluaran,
		PPNMasukan:       ppnMasukan,
		PPNTerutang:      ppnTerutang,
		PPh21Total:       pph21,
		EstimatedPayable: estimatedPayable,
		TotalTaxInvoices: taxInvoicesCount,
	}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// scopedAccounts projects balances exclusively from posted journal lines.
// Date boundaries are inclusive and blank boundaries are unbounded.
func (s *ReportService) scopedAccounts(companyID, branchID uint, from, to string) []domain.Account {
	accounts := s.store.ListAccounts(companyID)
	index := make(map[uint]int, len(accounts))
	for i := range accounts {
		accounts[i].Balance = 0
		index[accounts[i].ID] = i
	}
	entries := filterJournalEntries(s.store.ListJournalEntries(companyID, domain.JournalStatusPosted), branchID, from, to)
	for _, entry := range entries {
		for _, line := range entry.Lines {
			i, ok := index[line.AccountID]
			if !ok {
				continue
			}
			accounts[i].Balance += balanceDelta(accounts[i].Type, line.Debit, line.Credit)
		}
	}
	return accounts
}

func filterJournalEntries(entries []domain.JournalEntry, branchID uint, from, to string) []domain.JournalEntry {
	out := make([]domain.JournalEntry, 0, len(entries))
	for _, entry := range entries {
		if branchID != 0 && entry.BranchID != branchID {
			continue
		}
		if from != "" && entry.Date < from {
			continue
		}
		if to != "" && entry.Date > to {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// --- Receivables aging ---

// AgingBucket names one column of the aging matrix. Buckets are keyed by how
// many days past due an invoice is, not by how old it is, because that is what
// determines collection risk.
type AgingBucket struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Min   int    `json:"min"` // days past due, inclusive
	Max   int    `json:"max"` // days past due, exclusive; -1 means no upper bound
}

// AgingRow is one customer's exposure broken down by bucket.
type AgingRow struct {
	EntityID uint             `json:"entityId"`
	Name     string           `json:"name"`
	Buckets  map[string]int64 `json:"buckets"`
	Total    int64            `json:"total"`
}

// AgingReport is the AR aging matrix: rows per customer, columns per bucket.
type AgingReport struct {
	AsOf       string           `json:"asOf"`
	Buckets    []AgingBucket    `json:"buckets"`
	Rows       []AgingRow       `json:"rows"`
	Totals     map[string]int64 `json:"totals"`
	GrandTotal int64            `json:"grandTotal"`
}

// agingBuckets defines the standard receivables aging bands.
func agingBuckets() []AgingBucket {
	return []AgingBucket{
		{Key: "current", Label: "Current", Min: -1 << 30, Max: 1},
		{Key: "d1_30", Label: "1–30 days", Min: 1, Max: 31},
		{Key: "d31_60", Label: "31–60 days", Min: 31, Max: 61},
		{Key: "d61_90", Label: "61–90 days", Min: 61, Max: 91},
		{Key: "d90_plus", Label: "Over 90 days", Min: 91, Max: -1},
	}
}

// bucketFor returns the key of the bucket a given days-past-due falls into.
func bucketFor(buckets []AgingBucket, daysPastDue int) string {
	for _, b := range buckets {
		if daysPastDue < b.Min {
			continue
		}
		if b.Max != -1 && daysPastDue >= b.Max {
			continue
		}
		return b.Key
	}
	return buckets[len(buckets)-1].Key
}

// ReceivablesAging computes the AR aging matrix from outstanding invoices.
// Settled and draft invoices are excluded: drafts have not been issued, and
// paid invoices carry no collection risk.
func (s *ReportService) ReceivablesAging(companyID uint) (*AgingReport, error) {
	return s.ReceivablesAgingScoped(companyID, 0, "")
}

func (s *ReportService) ReceivablesAgingScoped(companyID, branchID uint, asOf string) (*AgingReport, error) {
	today := time.Now()
	if asOf != "" {
		parsed, err := time.Parse(dateLayout, asOf)
		if err != nil {
			return nil, domain.ErrValidation("asOf must use YYYY-MM-DD")
		}
		today = parsed
	}
	buckets := agingBuckets()

	rowsByCustomer := map[uint]*AgingRow{}
	order := []uint{}

	for _, inv := range s.store.ListInvoices(companyID) {
		if branchID != 0 && inv.BranchID != branchID {
			continue
		}
		if inv.Date > today.Format(dateLayout) {
			continue
		}
		if inv.Status == domain.InvoiceStatusDraft {
			continue
		}
		payments := s.store.ListInvoicePayments(companyID, inv.ID)
		outstanding := inv.Total
		for _, payment := range payments {
			if payment.Date <= today.Format(dateLayout) {
				outstanding -= payment.Amount
			}
		}
		if inv.Status == domain.InvoiceStatusPaid && len(payments) == 0 {
			outstanding = 0 // legacy seed before explicit payment records
		}
		if outstanding <= 0 {
			continue
		}

		row, seen := rowsByCustomer[inv.CustomerID]
		if !seen {
			row = &AgingRow{
				EntityID: inv.CustomerID,
				Name:     inv.CustomerName,
				Buckets:  map[string]int64{},
			}
			rowsByCustomer[inv.CustomerID] = row
			order = append(order, inv.CustomerID)
		}

		key := bucketFor(buckets, daysPastDue(inv.DueDate, today))
		row.Buckets[key] += outstanding
		row.Total += outstanding
	}

	report := &AgingReport{
		AsOf:    today.Format("2006-01-02"),
		Buckets: buckets,
		Rows:    make([]AgingRow, 0, len(order)),
		Totals:  map[string]int64{},
	}
	for _, id := range order {
		row := rowsByCustomer[id]
		// Guarantee every bucket key exists so the UI never renders "undefined".
		for _, b := range buckets {
			if _, ok := row.Buckets[b.Key]; !ok {
				row.Buckets[b.Key] = 0
			}
			report.Totals[b.Key] += row.Buckets[b.Key]
		}
		report.Rows = append(report.Rows, *row)
		report.GrandTotal += row.Total
	}

	// Largest exposure first — the rows a finance lead should look at.
	sort.SliceStable(report.Rows, func(i, j int) bool {
		return report.Rows[i].Total > report.Rows[j].Total
	})

	return report, nil
}

// daysPastDue returns how many days ago the due date passed. The result is
// negative while the invoice is still inside its payment terms.
func daysPastDue(dueDate string, now time.Time) int {
	due, err := time.Parse("2006-01-02", dueDate)
	if err != nil {
		return 0
	}
	midnightNow := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return int(midnightNow.Sub(due).Hours() / 24)
}

// --- Payables (removed scope) ---
//
// Purchasing / accounts-payable is out of scope for this portfolio release.
// The balance-sheet trade-payables balance remains the source of truth for
// what is owed; no separate AP-aging matrix is kept because vendor bills
// have no corresponding domain model in this release.
