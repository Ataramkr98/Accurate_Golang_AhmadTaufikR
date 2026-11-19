package service

import (
	"reflect"
	"testing"
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

type dashboardReadCounter struct {
	repository.Store
	accounts, journals, invoices int
}

func (s *dashboardReadCounter) ListAccounts(companyID uint) []domain.Account {
	s.accounts++
	return s.Store.ListAccounts(companyID)
}

func (s *dashboardReadCounter) ListJournalEntries(companyID uint, status string) []domain.JournalEntry {
	s.journals++
	return s.Store.ListJournalEntries(companyID, status)
}

func (s *dashboardReadCounter) ListInvoices(companyID uint) []domain.Invoice {
	s.invoices++
	return s.Store.ListInvoices(companyID)
}

func TestDashboardLoadsEachDatasetOnce(t *testing.T) {
	store := repository.NewMemoryStore()
	for _, branchID := range []uint{0, store.ListBranches(1)[0].ID} {
		for _, period := range []string{"", time.Now().Format("2006-01"), "2000-01"} {
			counter := &dashboardReadCounter{Store: store}
			dash, err := NewReportService(counter).DashboardScoped(1, branchID, period)
			if err != nil {
				t.Fatal(err)
			}
			if counter.accounts != 1 || counter.journals != 1 || counter.invoices != 1 {
				t.Errorf("branch=%d period=%q: reads accounts=%d journals=%d invoices=%d, want one each",
					branchID, period, counter.accounts, counter.journals, counter.invoices)
			}
			var from, to string
			if period != "" {
				month, _ := time.Parse("2006-01", period)
				from = month.Format(dateLayout)
				to = month.AddDate(0, 1, -1).Format(dateLayout)
			}
			reports := NewReportService(store)
			pl, err := reports.ProfitLossScoped(1, branchID, from, to)
			if err != nil {
				t.Fatal(err)
			}
			if dash.KPIs[2].Value != pl.NetIncome {
				t.Errorf("profit=%d, want %d", dash.KPIs[2].Value, pl.NetIncome)
			}
			for _, account := range reports.scopedAccounts(1, branchID, "", to) {
				if account.Code == "1100" && dash.KPIs[0].Value != account.Balance {
					t.Errorf("cash=%d, want %d", dash.KPIs[0].Value, account.Balance)
				}
			}
			// Selecting a historical KPI period must not trim the trailing chart.
			current, err := reports.DashboardScoped(1, branchID, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(dash.CashFlowSeries, current.CashFlowSeries) {
				t.Error("cash-flow series changed with the selected KPI period")
			}
		}
	}
}
