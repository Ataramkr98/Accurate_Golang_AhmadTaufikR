package repository

import (
	"errors"
	"testing"

	"tera/internal/domain"
)

func TestMemoryAccountBalancesAreDerivedFromPostedJournalLines(t *testing.T) {
	store := NewMemoryStore()
	var cashID uint
	for i := range store.accounts {
		if store.accounts[i].Code == "1100" {
			cashID = store.accounts[i].ID
			store.accounts[i].Balance = 9_999_999_999
			break
		}
	}
	if cashID == 0 {
		t.Fatal("seeded cash account not found")
	}
	cash, err := store.GetAccount(1, cashID)
	if err != nil {
		t.Fatal(err)
	}
	if cash.Balance == 9_999_999_999 {
		t.Fatal("account balance was read from the persisted cache instead of posted journal lines")
	}
}

func TestMemoryTransactionRollsBackDocumentNumberAndAudit(t *testing.T) {
	store := NewMemoryStore()
	err := store.WithTransaction(func(tx Store) error {
		tx.NextNumber(1, "INV")
		if err := tx.CreateCustomer(&domain.Customer{CompanyID: 1, Name: "Rollback Customer"}); err != nil {
			return err
		}
		if err := tx.AppendAuditLog(domain.AuditLog{CompanyID: 1, Action: "test.rollback", Entity: "test"}); err != nil {
			return err
		}
		return errors.New("force rollback")
	})
	if err == nil {
		t.Fatal("expected forced rollback error")
	}
	for _, customer := range store.ListCustomers(1) {
		if customer.Name == "Rollback Customer" {
			t.Fatal("customer committed despite transaction rollback")
		}
	}
	if got := store.NextNumber(1, "INV"); got != "INV-0242" {
		t.Fatalf("next invoice number = %q, want INV-0242 after rollback", got)
	}
	for _, entry := range store.ListAuditLogs(1) {
		if entry.Action == "test.rollback" {
			t.Fatal("audit row committed despite transaction rollback")
		}
	}
}
