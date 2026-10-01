package contract

import (
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
	"tera/internal/service"
)

func newJournalService(store repository.Store) *service.JournalService {
	return service.NewJournalService(store)
}

func newInvoiceService(store repository.Store) *service.InvoiceService {
	tax := service.NewTaxService()
	journals := service.NewJournalService(store)
	return service.NewInvoiceService(store, tax, journals)
}

func issueWithStore(store repository.Store, _ *service.InvoiceService, companyID, invID uint) error {
	svc := newInvoiceService(store)
	_, err := svc.Issue(companyID, 1, invID)
	return err
}

func payWithStore(store repository.Store, _ *service.InvoiceService, companyID, invID, cashID uint, amount int64) (*domain.Invoice, error) {
	svc := newInvoiceService(store)
	return svc.RecordPayment(companyID, 1, invID, service.InvoicePaymentInput{
		CashAccountID: cashID,
		Amount:        amount,
		Date:          time.Now().Format("2006-01-02"),
	})
}

func journalInput(cash, revenue uint, amount int64) service.JournalInput {
	return service.JournalInput{
		Memo: "contract draft",
		Lines: []service.JournalLineInput{
			{AccountID: cash, Debit: amount},
			{AccountID: revenue, Credit: amount},
		},
	}
}

func invoiceInput(companyID uint) service.InvoiceInput {
	return service.InvoiceInput{
		CustomerName: "PT Contract Customer",
		Date:         "2026-09-21",
		DueDate:      "2026-10-21",
		Notes:        "contract invoice",
		Lines: []service.InvoiceLineInput{
			{Name: "Contract line", Qty: 1, Price: 1_000_000},
		},
	}
}

func countAction(logs []domain.AuditLog, action string) int {
	n := 0
	for _, l := range logs {
		if l.Action == action {
			n++
		}
	}
	return n
}
