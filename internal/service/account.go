package service

import (
	"strings"

	"tera/internal/domain"
	"tera/internal/repository"
)

// AccountService exposes chart-of-accounts operations.
type AccountService struct {
	store repository.Store
}

func NewAccountService(store repository.Store) *AccountService {
	return &AccountService{store: store}
}

// AccountInput is the client payload for creating/updating an account.
type AccountInput struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	ParentID uint   `json:"parentId"`
	IsActive *bool  `json:"isActive"`
}

func (s *AccountService) List(companyID uint) []domain.Account {
	return s.store.ListAccounts(companyID)
}

func (s *AccountService) Get(companyID, id uint) (*domain.Account, error) {
	return s.store.GetAccount(companyID, id)
}

func (s *AccountService) Create(companyID uint, input AccountInput) (*domain.Account, error) {
	code := strings.TrimSpace(input.Code)
	name := strings.TrimSpace(input.Name)
	typ := normalizeType(strings.TrimSpace(input.Type))
	if code == "" || name == "" {
		return nil, domain.ErrValidation("account code and name are required")
	}
	if !validAccountType(typ) {
		return nil, domain.ErrValidation("invalid account type")
	}

	parentID := input.ParentID
	if parentID == 0 {
		for _, candidate := range s.store.ListAccounts(companyID) {
			if candidate.ParentID == 0 && candidate.Type == typ {
				parentID = candidate.ID
				break
			}
		}
		if parentID == 0 {
			return nil, domain.ErrValidation("parent group for this account type was not found")
		}
	} else {
		parent, err := s.store.GetAccount(companyID, parentID)
		if err != nil {
			return nil, err
		}
		if parent.ParentID != 0 || parent.Type != typ {
			return nil, domain.ErrValidation("parent account must be a group of the same type")
		}
	}

	acc := &domain.Account{
		CompanyID: companyID,
		Code:      code,
		Name:      name,
		Type:      typ,
		ParentID:  parentID,
		IsSystem:  false,
		IsActive:  true,
		Balance:   0,
	}
	if err := s.store.CreateAccount(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

func (s *AccountService) Update(companyID, id uint, input AccountInput) (*domain.Account, error) {
	acc, err := s.store.GetAccount(companyID, id)
	if err != nil {
		return nil, err
	}
	if acc.IsSystem && (input.Code != "" && input.Code != acc.Code || input.Type != "" && normalizeType(input.Type) != acc.Type) {
		return nil, domain.ErrForbidden("system account code and type cannot be changed")
	}
	if name := strings.TrimSpace(input.Name); name != "" {
		acc.Name = name
	}
	if input.IsActive != nil {
		acc.IsActive = *input.IsActive
	}
	if err := s.store.UpdateAccount(acc); err != nil {
		return nil, err
	}
	return acc, nil
}

func (s *AccountService) Delete(companyID, id uint) error {
	return s.store.DeleteAccount(companyID, id)
}

// EnsureDefaults creates the minimum complete chart of accounts needed by
// journals, invoicing, tax posting, and the core financial statements.
func (s *AccountService) EnsureDefaults(companyID uint) error {
	type defaultAccount struct {
		code, name, typ, parentCode string
		isSystem                    bool
	}
	defaults := []defaultAccount{
		{code: "1000", name: "ASSETS", typ: domain.TypeAsset, isSystem: true},
		{code: "2000", name: "LIABILITIES", typ: domain.TypeLiability, isSystem: true},
		{code: "3000", name: "EQUITY", typ: domain.TypeEquity, isSystem: true},
		{code: "4000", name: "REVENUE", typ: domain.TypeRevenue, isSystem: true},
		{code: "5000", name: "EXPENSES", typ: domain.TypeExpense, isSystem: true},
		{code: "6000", name: "COST OF GOODS SOLD", typ: domain.TypeExpense, isSystem: true},
		{code: "1100", name: "Cash & Cash Equivalents", typ: domain.TypeAsset, parentCode: "1000", isSystem: true},
		{code: "1200", name: "Accounts Receivable", typ: domain.TypeAsset, parentCode: "1000", isSystem: true},
		{code: "1300", name: "Merchandise Inventory", typ: domain.TypeAsset, parentCode: "1000"},
		{code: "1600", name: "Fixed Assets", typ: domain.TypeAsset, parentCode: "1000"},
		{code: "1690", name: "Accumulated Depreciation", typ: domain.TypeAsset, parentCode: "1000", isSystem: true},
		{code: "2100", name: "Accounts Payable", typ: domain.TypeLiability, parentCode: "2000", isSystem: true},
		{code: "2200", name: "VAT Payable", typ: domain.TypeLiability, parentCode: "2000", isSystem: true},
		{code: "3100", name: "Paid-In Capital", typ: domain.TypeEquity, parentCode: "3000"},
		{code: "3200", name: "Retained Earnings", typ: domain.TypeEquity, parentCode: "3000", isSystem: true},
		{code: "4100", name: "Product Sales", typ: domain.TypeRevenue, parentCode: "4000", isSystem: true},
		{code: "4200", name: "Service Revenue", typ: domain.TypeRevenue, parentCode: "4000"},
		{code: "5100", name: "Salaries & Wages Expense", typ: domain.TypeExpense, parentCode: "5000"},
		{code: "5200", name: "Rent Expense", typ: domain.TypeExpense, parentCode: "5000"},
		{code: "5300", name: "Utilities Expense", typ: domain.TypeExpense, parentCode: "5000"},
		{code: "5400", name: "Depreciation Expense", typ: domain.TypeExpense, parentCode: "5000", isSystem: true},
		{code: "5900", name: "Other Operating Expenses", typ: domain.TypeExpense, parentCode: "5000"},
		{code: "6100", name: "Cost of Goods Sold", typ: domain.TypeExpense, parentCode: "6000"},
	}

	for _, item := range defaults {
		if _, err := s.store.GetAccountByCode(companyID, item.code); err == nil {
			continue
		}
		parentID := uint(0)
		if item.parentCode != "" {
			parent, err := s.store.GetAccountByCode(companyID, item.parentCode)
			if err != nil {
				return err
			}
			parentID = parent.ID
		}
		if err := s.store.CreateAccount(&domain.Account{
			CompanyID: companyID,
			Code:      item.code,
			Name:      item.name,
			Type:      item.typ,
			ParentID:  parentID,
			IsSystem:  item.isSystem,
			IsActive:  true,
		}); err != nil {
			return err
		}
	}
	return nil
}

func validAccountType(typ string) bool {
	switch typ {
	case domain.TypeAsset, domain.TypeLiability, domain.TypeEquity, domain.TypeRevenue, domain.TypeExpense:
		return true
	default:
		return false
	}
}

// normalizeType maps display labels (English or legacy Indonesian) to the internal type key.
func normalizeType(typ string) string {
	switch typ {
	case domain.TypeAssetLabel, "Aset", "asset":
		return domain.TypeAsset
	case domain.TypeLiabilityLabel, "Liabilitas", "liability":
		return domain.TypeLiability
	case domain.TypeEquityLabel, "Ekuitas", "equity":
		return domain.TypeEquity
	case domain.TypeRevenueLabel, "Pendapatan", "revenue":
		return domain.TypeRevenue
	case domain.TypeExpenseLabel, "Beban", "expense":
		return domain.TypeExpense
	default:
		return typ
	}
}

// TypeLabel returns the display label for a type key.
func TypeLabel(typ string) string {
	switch typ {
	case domain.TypeAsset:
		return domain.TypeAssetLabel
	case domain.TypeLiability:
		return domain.TypeLiabilityLabel
	case domain.TypeEquity:
		return domain.TypeEquityLabel
	case domain.TypeRevenue:
		return domain.TypeRevenueLabel
	case domain.TypeExpense:
		return domain.TypeExpenseLabel
	default:
		return typ
	}
}
