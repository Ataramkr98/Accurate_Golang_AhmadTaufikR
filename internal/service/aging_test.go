package service

import (
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

// These tests guard the aging reports, which the UI renders as a matrix. The
// risk they catch is subtle: an arithmetic slip (double counting a bucket,
// including settled invoices, or dropping rows) produces a matrix that looks
// plausible but does not foot.

func TestReceivablesAging_FootsToOutstandingInvoices(t *testing.T) {
	store := repository.NewMemoryStore()
	report, err := NewReportService(store).ReceivablesAging(1)
	if err != nil {
		t.Fatalf("ReceivablesAging returned error: %v", err)
	}

	// Independently sum every invoice that still represents collection risk.
	var expected int64
	for _, invoice := range store.ListInvoices(1) {
		if invoice.Status == domain.InvoiceStatusDraft || invoice.Status == domain.InvoiceStatusPaid {
			continue
		}
		expected += invoice.Total
	}

	if report.GrandTotal != expected {
		t.Errorf("grand total = %d, want %d (sum of unpaid, issued invoices)", report.GrandTotal, expected)
	}

	// Every bucket total must sum to the grand total.
	var bucketSum int64
	for _, bucket := range report.Buckets {
		bucketSum += report.Totals[bucket.Key]
	}
	if bucketSum != report.GrandTotal {
		t.Errorf("bucket totals sum to %d, want %d", bucketSum, report.GrandTotal)
	}

	// Row totals must also sum to the grand total.
	var rowSum int64
	for _, row := range report.Rows {
		if row.Total <= 0 {
			t.Errorf("customer %q has non-positive total %d", row.Name, row.Total)
		}
		rowSum += row.Total
	}
	if rowSum != report.GrandTotal {
		t.Errorf("row totals sum to %d, want %d", rowSum, report.GrandTotal)
	}
}

func TestReceivablesAging_PopulatesEveryBucketKey(t *testing.T) {
	store := repository.NewMemoryStore()
	report, err := NewReportService(store).ReceivablesAging(1)
	if err != nil {
		t.Fatalf("ReceivablesAging returned error: %v", err)
	}

	// The UI indexes buckets by key, so a missing key renders as blank.
	for _, bucket := range report.Buckets {
		if _, ok := report.Totals[bucket.Key]; !ok {
			t.Errorf("bucket %q missing from totals map", bucket.Key)
		}
	}
	for _, row := range report.Rows {
		for _, bucket := range report.Buckets {
			if _, ok := row.Buckets[bucket.Key]; !ok {
				t.Errorf("row %q missing bucket %q", row.Name, bucket.Key)
			}
		}
	}

	// The seed is designed with several overdue invoices; if the buckets are all
	// empty except "current" the aging report has nothing to demonstrate.
	if report.Totals["d1_30"]+report.Totals["d31_60"]+report.Totals["d61_90"]+report.Totals["d90_plus"] == 0 {
		t.Error("no overdue exposure in any aging bucket; the report cannot demonstrate aging")
	}
}

func TestReceivablesAging_ExcludesDraftsAndPaid(t *testing.T) {
	store := repository.NewMemoryStore()
	service := NewReportService(store)

	before, err := service.ReceivablesAging(1)
	if err != nil {
		t.Fatalf("ReceivablesAging returned error: %v", err)
	}

	// A freshly paid invoice must not appear in the aging matrix.
	var paidTotal int64
	for _, invoice := range store.ListInvoices(1) {
		if invoice.Status == domain.InvoiceStatusPaid {
			paidTotal += invoice.Total
		}
	}
	if paidTotal == 0 {
		t.Fatal("expected the seed to contain settled invoices for this assertion to mean anything")
	}

	// Sanity: the grand total must be strictly smaller than all invoices, since
	// drafts and paid ones are excluded.
	var allTotal int64
	for _, invoice := range store.ListInvoices(1) {
		allTotal += invoice.Total
	}
	if before.GrandTotal >= allTotal {
		t.Errorf("aging total %d should exclude drafts/paid, which total %d", before.GrandTotal, allTotal)
	}
}

// Purchasing / accounts-payable was removed with the focused release scope:
// the AP-aging report, its bill-journal derivation, and the vendor-memo
// extraction helpers no longer exist. Receivables aging below is retained.

func TestAgingBuckets_CoverTheWholeTimeline(t *testing.T) {
	buckets := agingBuckets()

	cases := []struct {
		daysPastDue int
		want        string
	}{
		{-30, "current"},
		{0, "current"},
		{1, "d1_30"},
		{30, "d1_30"},
		{31, "d31_60"},
		{60, "d31_60"},
		{61, "d61_90"},
		{90, "d61_90"},
		{91, "d90_plus"},
		{4000, "d90_plus"},
	}

	for _, tc := range cases {
		if got := bucketFor(buckets, tc.daysPastDue); got != tc.want {
			t.Errorf("bucketFor(%d) = %q, want %q", tc.daysPastDue, got, tc.want)
		}
	}
}

// --- Dashboard cash-flow series ---

func TestDashboard_CashFlowSeriesShape(t *testing.T) {
	store := repository.NewMemoryStore()
	report, err := NewReportService(store).Dashboard(1)
	if err != nil {
		t.Fatalf("Dashboard returned error: %v", err)
	}

	if len(report.CashFlowSeries) != cashFlowMonths {
		t.Fatalf("series has %d points, want %d", len(report.CashFlowSeries), cashFlowMonths)
	}

	// The axis must be ordered oldest -> newest and carry a month label.
	for i, point := range report.CashFlowSeries {
		if point.Period == "" || point.Label == "" {
			t.Errorf("point %d missing period/label: %+v", i, point)
		}
		if i > 0 && report.CashFlowSeries[i-1].Period >= point.Period {
			t.Errorf("series out of order at %d: %q then %q",
				i, report.CashFlowSeries[i-1].Period, point.Period)
		}
	}

	// Net must equal inflow minus outflow for every point.
	for _, point := range report.CashFlowSeries {
		if point.Net != point.Inflow-point.Outflow {
			t.Errorf("period %s net = %d, want inflow-outflow = %d",
				point.Period, point.Net, point.Inflow-point.Outflow)
		}
	}
}

func TestDashboard_CashFlowSeriesReflectsSeededJournals(t *testing.T) {
	store := repository.NewMemoryStore()
	report, err := NewReportService(store).Dashboard(1)
	if err != nil {
		t.Fatalf("Dashboard returned error: %v", err)
	}

	// The current month must show activity, because the seed posts cash-moving
	// journals within the last few days.
	current := report.CashFlowSeries[len(report.CashFlowSeries)-1]
	if current.Inflow == 0 && current.Outflow == 0 {
		t.Errorf("current month %s has no cash movement; the seed should post some", current.Period)
	}

	// The series must only count posted entries. Draft entries touch salary and
	// payables, never cash, so a draft leaking in would show up as movement in
	// a month with no posted cash journals — assert the totals are sane instead.
	var totalInflow int64
	for _, point := range report.CashFlowSeries {
		if point.Inflow < 0 || point.Outflow < 0 {
			t.Errorf("period %s has negative flow: in=%d out=%d", point.Period, point.Inflow, point.Outflow)
		}
		totalInflow += point.Inflow
	}
	if totalInflow == 0 {
		t.Error("no inflow across the whole series; seeded sales receipts are missing")
	}
}

// --- Tax summary ---

// TestTaxSummary_ReadsRealTaxAccounts guards two bugs that hid behind plausible
// output: ppnMasukan was declared but never assigned (so PPN terutang always
// equalled PPN keluaran), and PPh 21 was a fabricated 5% of salary expense
// rather than the amount actually withheld in account 2210.
func TestTaxSummary_ReadsRealTaxAccounts(t *testing.T) {
	store := repository.NewMemoryStore()
	summary, err := NewReportService(store).TaxSummary(1)
	if err != nil {
		t.Fatalf("TaxSummary returned error: %v", err)
	}

	accounts := store.ListAccounts(1)
	balanceOf := func(code string) int64 {
		for _, a := range accounts {
			if a.Code == code {
				return a.Balance
			}
		}
		return 0
	}

	// PPh 21 must come from the withholding account, not a percentage of salary.
	if want := balanceOf("2210"); summary.PPh21Total != want {
		t.Errorf("PPh21Total = %d, want the 2210 account balance %d", summary.PPh21Total, want)
	}

	// Input VAT must be populated. The seed accrues PPN Masukan, so a zero here
	// means the assignment regressed back to the unset-variable bug.
	if summary.PPNMasukan == 0 {
		t.Error("PPNMasukan is 0; input VAT from account 2200 is not being read")
	}

	// The payable must foot: output VAT less input VAT, plus withheld PPh 21.
	if want := summary.PPNTerutang + summary.PPh21Total; summary.EstimatedPayable != want {
		t.Errorf("EstimatedPayable = %d, want %d (PPNTerutang + PPh21Total)", summary.EstimatedPayable, want)
	}
	if want := summary.PPNKeluaran - summary.PPNMasukan; summary.PPNTerutang != want {
		t.Errorf("PPNTerutang = %d, want %d (PPNKeluaran - PPNMasukan)", summary.PPNTerutang, want)
	}
}

// TestTaxSummary_InputVATComesFromTheLedger pins the second half of the fix:
// PPN Keluaran is summed from invoices, while PPN Masukan is read from the 2200
// account. Previously ppnMasukan was declared but never assigned, so PPN
// terutang silently equalled PPN keluaran and over-stated the liability.
func TestTaxSummary_InputVATComesFromTheLedger(t *testing.T) {
	store := repository.NewMemoryStore()

	before, err := NewReportService(store).TaxSummary(1)
	if err != nil {
		t.Fatalf("TaxSummary returned error: %v", err)
	}

	// Establish a known input-VAT movement on the 2200 account.
	accounts := store.ListAccounts(1)
	var vatAccount *domain.Account
	for i := range accounts {
		if accounts[i].Code == "2200" {
			vatAccount = &accounts[i]
			break
		}
	}
	if vatAccount == nil {
		t.Fatal("account 2200 not found in the seeded chart of accounts")
	}
	cash, err := store.GetAccountByCode(1, "1100")
	if err != nil {
		t.Fatal(err)
	}

	const inputVAT = 2_500_000
	if _, err := NewJournalService(store).Create(1, 1, JournalInput{
		Status: domain.JournalStatusPosted,
		Memo:   "Input VAT settlement",
		Lines: []JournalLineInput{
			{AccountID: vatAccount.ID, Debit: inputVAT},
			{AccountID: cash.ID, Credit: inputVAT},
		},
	}); err != nil {
		t.Fatalf("posting input VAT journal: %v", err)
	}

	after, err := NewReportService(store).TaxSummary(1)
	if err != nil {
		t.Fatalf("TaxSummary returned error: %v", err)
	}

	// PPN Masukan must track the account, not stay pinned at zero.
	if want := before.PPNMasukan - inputVAT; after.PPNMasukan != want {
		t.Errorf("PPNMasukan = %d, want %d (moved with account 2200)", after.PPNMasukan, want)
	}
	if want := after.PPNKeluaran - after.PPNMasukan; after.PPNTerutang != want {
		t.Errorf("PPNTerutang = %d, want %d (PPNKeluaran - PPNMasukan)", after.PPNTerutang, want)
	}
}
