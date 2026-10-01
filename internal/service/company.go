package service

import (
	"strings"

	"tera/internal/domain"
	"tera/internal/repository"
)

type CompanyService struct {
	store    repository.Store
	accounts *AccountService
}

func NewCompanyService(store repository.Store, accounts *AccountService) *CompanyService {
	return &CompanyService{store: store, accounts: accounts}
}

type CompanyInput struct {
	Name         string `json:"name"`
	NPWP         string `json:"npwp"`
	SAKMode      string `json:"sakMode"`
	IsPKP        *bool  `json:"isPkp"`
	Address      string `json:"address"`
	BusinessType string `json:"businessType"`
}

func (s *CompanyService) Update(companyID uint, input CompanyInput) (*domain.Company, error) {
	company, err := s.store.GetCompany(companyID)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, domain.ErrValidation("company name is required")
	}
	sakMode := normalizeSAKMode(input.SAKMode)
	if sakMode == "" {
		return nil, domain.ErrValidation("invalid SAK mode")
	}

	company.Name = name
	company.NPWP = strings.TrimSpace(input.NPWP)
	company.SAKMode = sakMode
	company.Address = strings.TrimSpace(input.Address)
	company.BusinessType = strings.TrimSpace(input.BusinessType)
	if input.IsPKP != nil {
		company.IsPKP = *input.IsPKP
	}
	if err := s.store.UpdateCompany(company); err != nil {
		return nil, err
	}
	if err := s.accounts.EnsureDefaults(companyID); err != nil {
		return nil, err
	}
	return company, nil
}

func (s *CompanyService) ListBranches(companyID uint) []domain.Branch {
	return s.store.ListBranches(companyID)
}

func (s *CompanyService) GetBranch(companyID, id uint) (*domain.Branch, error) {
	return s.store.GetBranch(companyID, id)
}

func (s *CompanyService) CreateBranch(companyID uint, name string) (*domain.Branch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, domain.ErrValidation("branch name is required")
	}
	b := &domain.Branch{CompanyID: companyID, Name: name}
	if err := s.store.CreateBranch(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *CompanyService) UpdateBranch(companyID, id uint, name string) (*domain.Branch, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, domain.ErrValidation("branch name is required")
	}
	b, err := s.store.GetBranch(companyID, id)
	if err != nil {
		return nil, err
	}
	b.Name = name
	if err := s.store.UpdateBranch(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (s *CompanyService) DeleteBranch(companyID, id uint) error {
	branches := s.store.ListBranches(companyID)
	if len(branches) <= 1 {
		return domain.ErrValidation("company must have at least one branch")
	}
	return s.store.DeleteBranch(companyID, id)
}

func normalizeSAKMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case domain.SAKEMKM:
		return domain.SAKEMKM
	case domain.SAKPSAK, "psak":
		return domain.SAKPSAK
	default:
		return ""
	}
}
