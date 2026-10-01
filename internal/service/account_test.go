package service

import (
	"testing"

	"tera/internal/domain"
	"tera/internal/repository"
)

func TestAccountCreate_AttachesToTypeHeader(t *testing.T) {
	store := repository.NewMemoryStore()
	service := NewAccountService(store)

	account, err := service.Create(1, AccountInput{Code: "1199", Name: "Kas Proyek", Type: "Aset"})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if account.ParentID == 0 {
		t.Fatal("custom account was created as a root header")
	}
	parent, err := store.GetAccount(1, account.ParentID)
	if err != nil {
		t.Fatalf("get parent: %v", err)
	}
	if parent.ParentID != 0 || parent.Type != domain.TypeAsset {
		t.Fatalf("unexpected parent: %#v", parent)
	}
}

func TestAccountUpdate_OmittedActiveStateDoesNotDeactivate(t *testing.T) {
	store := repository.NewMemoryStore()
	service := NewAccountService(store)
	account, err := store.GetAccountByCode(1, "1300")
	if err != nil {
		t.Fatal(err)
	}

	updated, err := service.Update(1, account.ID, AccountInput{Name: "Persediaan Barang"})
	if err != nil {
		t.Fatalf("update account: %v", err)
	}
	if !updated.IsActive {
		t.Fatal("omitting isActive unexpectedly deactivated the account")
	}
}
