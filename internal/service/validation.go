package service

import (
	"time"

	"tera/internal/domain"
	"tera/internal/repository"
)

const dateLayout = "2006-01-02"

func normalizeDate(value string, fallback time.Time) (string, time.Time, error) {
	if value == "" {
		value = fallback.Format(dateLayout)
	}
	parsed, err := time.Parse(dateLayout, value)
	if err != nil {
		return "", time.Time{}, domain.ErrValidation("date must use YYYY-MM-DD format")
	}
	return parsed.Format(dateLayout), parsed, nil
}

func resolveBranch(store repository.Store, companyID, requestedID uint) (uint, error) {
	branches := store.ListBranches(companyID)
	if len(branches) == 0 {
		return 0, domain.ErrValidation("company has no branches yet")
	}
	if requestedID == 0 {
		return branches[0].ID, nil
	}
	for _, branch := range branches {
		if branch.ID == requestedID {
			return branch.ID, nil
		}
	}
	return 0, domain.ErrValidation("invalid branch")
}
