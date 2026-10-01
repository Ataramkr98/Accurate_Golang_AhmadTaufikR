package service

import (
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

func TestJournalCreate_Balanced(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}

	// Capture the seeded position so assertions describe the movement rather
	// than restating the seed values.
	cashAcc, _ := store.GetAccount(1, cash)
	revenueAcc, _ := store.GetAccount(1, revenue)
	cashBefore := cashAcc.Balance
	revenueBefore := revenueAcc.Balance

	e, err := js.Create(1, 1, JournalInput{
		Memo:   "kas vs pendapatan",
		Status: domain.JournalStatusPosted,
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 1_000_000},
			{AccountID: revenue, Credit: 1_000_000},
		},
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if e.Status != domain.JournalStatusPosted {
		t.Errorf("status = %q, want %q", e.Status, domain.JournalStatusPosted)
	}
	if len(e.Lines) != 2 {
		t.Errorf("lines = %d, want 2", len(e.Lines))
	}
	if e.Number == "" {
		t.Error("number should be assigned")
	}

	// Balances should have moved by exactly the posted amount. Account types
	// must be fetched fresh because GetAccount returns a copy.
	cashAfter, _ := store.GetAccount(1, cash)
	revenueAfter, _ := store.GetAccount(1, revenue)
	if cashAfter.Balance != cashBefore+1_000_000 {
		t.Errorf("cash balance = %d, want %d", cashAfter.Balance, cashBefore+1_000_000)
	}
	if revenueAfter.Balance != revenueBefore+1_000_000 {
		t.Errorf("revenue balance = %d, want %d", revenueAfter.Balance, revenueBefore+1_000_000)
	}
}

func TestJournalCreate_NotBalanced(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}

	_, err := js.Create(1, 1, JournalInput{
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 1_000_000},
			{AccountID: revenue, Credit: 999_999},
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	dErr, ok := err.(*domain.Error)
	if !ok {
		t.Fatalf("expected *domain.Error, got %T", err)
	}
	if dErr.Code != domain.CodeJournalNotBalanced {
		t.Errorf("code = %q, want %q", dErr.Code, domain.CodeJournalNotBalanced)
	}
}

func TestJournalCreate_RejectsBothDebitAndCredit(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash uint
	for _, a := range accs {
		if a.Code == "1100" {
			cash = a.ID
		}
	}

	_, err := js.Create(1, 1, JournalInput{
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 100, Credit: 100},
		},
	})
	if err == nil {
		t.Fatal("expected error for both debit and credit on one line")
	}
}

func TestJournalCreate_DefaultsToDraft(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}

	entry, err := js.Create(1, 1, JournalInput{
		Memo: "Draft adjustment",
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 100_000},
			{AccountID: revenue, Credit: 100_000},
		},
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if entry.Status != domain.JournalStatusDraft {
		t.Fatalf("status = %q, want %q", entry.Status, domain.JournalStatusDraft)
	}
}

func TestJournalDraftLifecycle_PostsOnceAndBecomesImmutable(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}

	draft, err := js.Create(1, 1, JournalInput{
		Memo: "Initial draft",
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 250_000},
			{AccountID: revenue, Credit: 250_000},
		},
	})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	updated, err := js.Update(1, 1, draft.ID, JournalInput{
		Memo: "Updated draft",
		Lines: []JournalLineInput{
			{AccountID: cash, Debit: 300_000},
			{AccountID: revenue, Credit: 300_000},
		},
	})
	if err != nil {
		t.Fatalf("update draft: %v", err)
	}
	if updated.Memo != "Updated draft" {
		t.Fatalf("memo = %q, want Updated draft", updated.Memo)
	}

	posted, err := js.Post(1, 1, draft.ID)
	if err != nil {
		t.Fatalf("post draft: %v", err)
	}
	if posted.Status != domain.JournalStatusPosted {
		t.Fatalf("status = %q, want %q", posted.Status, domain.JournalStatusPosted)
	}
	if _, err := js.Post(1, 1, draft.ID); err == nil {
		t.Fatal("posting an already posted journal should fail")
	}
	if _, err := js.Update(1, 1, draft.ID, JournalInput{Memo: "No longer editable"}); err == nil {
		t.Fatal("updating a posted journal should fail")
	}
	if err := js.Delete(1, 1, draft.ID); err == nil {
		t.Fatal("deleting a posted journal should fail")
	}
}

func TestJournalDraftLifecycle_DeleteDraft(t *testing.T) {
	store := repository.NewMemoryStore()
	js := NewJournalService(store)
	accs := store.ListAccounts(1)
	var cash, revenue uint
	for _, a := range accs {
		switch a.Code {
		case "1100":
			cash = a.ID
		case "4100":
			revenue = a.ID
		}
	}

	draft, err := js.Create(1, 1, JournalInput{Lines: []JournalLineInput{
		{AccountID: cash, Debit: 50_000},
		{AccountID: revenue, Credit: 50_000},
	}})
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}
	if err := js.Delete(1, 1, draft.ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := js.Get(1, draft.ID); err == nil {
		t.Fatal("deleted draft should not be found")
	}
}
