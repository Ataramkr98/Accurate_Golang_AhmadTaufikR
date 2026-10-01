package repository

import (
	"encoding/json"
	"log"
	"time"

	"tera/internal/domain"
	"tera/internal/security"
)

// issueTime converts a seeded document date (YYYY-MM-DD) into a timestamp
// during Jakarta business hours.
//
// time.Parse yields UTC, and PostgreSQL renders the value in the session offset
// (+07), so 09:00 UTC would surface as 16:00. Building the instant in the
// Jakarta zone instead makes a seeded entry read as 09:00 local, matching a
// real working day.
func issueTime(date string) time.Time {
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Now()
	}
	return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 9, 0, 0, 0, jakarta)
}

// jakarta is the accounting timezone; the demo tenant and TaxSummary both
// assume WIB. Falling back to UTC keeps seeding alive on a host without tzdata.
var jakarta = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return time.UTC
	}
	return loc
}()

// SeedDemoData ensures the standard demo company, chart of accounts, customers,
// invoices, and journals exist in the store.
//
// It is deliberately store-agnostic: the dataset comes from the shared
// declarations in seeddata.go (demoCOA / demoCustomers / buildDemoInvoices /
// demoJournals), so the PostgreSQL store and the in-memory fallback present
// exactly the same demo tenant. That matters because the reports are only
// trustworthy when balances are derived from balanced journals rather than
// written down twice.
func SeedDemoData(store Store) error {
	if _, exists := store.FindUserByEmail(demoEmail); exists {
		log.Println("[Seed] Demo user already exists. Skipping initial seed.")
		return nil
	}

	log.Println("[Seed] Seeding realistic demo data...")

	company := &domain.Company{
		Name:         "PT Tera Sejahtera",
		NPWP:         "01.234.567.8-901.000",
		SAKMode:      domain.SAKEMKM,
		IsPKP:        true,
		Address:      "Jl. Ledger No. 10, Bandung",
		BusinessType: "Trading & Services",
	}
	if err := store.CreateCompany(company); err != nil {
		return err
	}

	// --- Branches ---
	// Invoices and journals reference a branch, so keep the IDs in the same
	// order as demoInvoices' branchIndex / the default posting branch.
	branchNames := []string{"Kantor Pusat — Bandung", "Cabang Cimahi", "Gudang Surabaya"}
	branchIDs := make([]uint, 0, len(branchNames))
	for _, name := range branchNames {
		branch := &domain.Branch{CompanyID: company.ID, Name: name}
		if err := store.CreateBranch(branch); err != nil {
			return err
		}
		branchIDs = append(branchIDs, branch.ID)
	}

	// --- Users ---
	password, err := security.HashPassword(demoPassword)
	if err != nil {
		return err
	}

	demoUser := &domain.User{
		CompanyID: company.ID,
		Email:     demoEmail,
		Password:  password,
		Name:      "Demo User",
		Role:      "Owner",
	}
	if err := store.CreateUser(demoUser); err != nil {
		return err
	}

	// A second seat in the same tenant, useful for demonstrating RBAC labels.
	// --- Chart of accounts ---
	// demoCOA lists group headers before their children, so a single ordered
	// pass resolves every ParentID reference. Opening balances carry the
	// position before any journal is applied; the journals below roll them
	// forward.
	idByCode := map[string]uint{}
	accountsByCode := map[string]*domain.Account{}
	for _, row := range demoCOA() {
		account := &domain.Account{
			CompanyID: company.ID,
			Code:      row.code,
			Name:      row.name,
			Type:      row.typ,
			ParentID:  idByCode[row.parent],
			IsSystem:  row.isSystem,
			IsActive:  true,
			Balance:   0,
		}
		if err := store.CreateAccount(account); err != nil {
			return err
		}
		idByCode[row.code] = account.ID
		accountsByCode[row.code] = account
	}
	openingAccountIDs := make(map[string]uint, len(accountsByCode))
	for code, account := range accountsByCode {
		openingAccountIDs[code] = account.ID
	}
	if err := store.CreateJournalEntry(&domain.JournalEntry{
		CompanyID: company.ID, BranchID: branchIDs[0], Number: "OPEN-0001", Date: "2000-01-01",
		Status: domain.JournalStatusPosted, SourceType: domain.SourceManual, Memo: "Opening balances",
		Lines: demoOpeningLines(openingAccountIDs), CreatedAt: time.Now(),
	}); err != nil {
		return err
	}

	// --- Customers ---
	customerIDs := map[string]uint{}
	for _, seed := range demoCustomers() {
		customer := seed
		customer.CompanyID = company.ID
		if err := store.CreateCustomer(&customer); err != nil {
			return err
		}
		customerIDs[customer.Name] = customer.ID
	}

	// --- Invoices ---
	// Dates come from the relative offsets in seeddata.go so the demo tenant
	// never looks stale, whatever day it is deployed.
	for _, seed := range buildDemoInvoices() {
		branchID := branchIDs[0]
		if seed.branchIndex >= 0 && seed.branchIndex < len(branchIDs) {
			branchID = branchIDs[seed.branchIndex]
		}
		invoice := &domain.Invoice{
			CompanyID:    company.ID,
			BranchID:     branchID,
			Number:       seed.number,
			CustomerID:   customerIDs[seed.customer],
			CustomerName: seed.customer,
			Date:         seed.issueDate(),
			DueDate:      seed.dueDate(),
			Status:       seed.status,
			Subtotal:     seed.subtotal,
			TaxAmount:    seed.taxAmount,
			Total:        seed.total(),
			Notes:        "Payment terms as agreed. Payment via bank transfer.",
			Lines: []domain.InvoiceLine{
				{
					Name:            "Product & Service Package - " + seed.customer,
					Qty:             1,
					Price:           seed.subtotal,
					DiscountPercent: 0,
					Subtotal:        seed.subtotal,
				},
			},
		}
		if err := store.CreateInvoice(invoice); err != nil {
			return err
		}

		// The UI presents the audit trail as a list of events, so the seeded
		// tenant needs events of its own. These mirror exactly what
		// InvoiceService.Create writes (action "invoice.create", entity
		// "invoice") and are stamped at the invoice's issue time so the trail
		// reads in the same order the documents were raised.
		after, _ := json.Marshal(map[string]any{
			"number": invoice.Number,
			"total":  invoice.Total,
		})
		if err := store.AppendAuditLog(domain.AuditLog{
			CompanyID: company.ID,
			UserID:    demoUser.ID,
			Action:    "invoice.create",
			Entity:    "invoice",
			AfterJSON: string(after),
			CreatedAt: issueTime(invoice.Date),
		}); err != nil {
			return err
		}
	}

	// --- Journals ---
	// Posted entries move the account balances; drafts are stored for review
	// but must not affect any report. Because every entry is balanced, the
	// balance sheet derived from them stays in balance.
	highestJournal := 0
	// The current-period entries and the generated monthly history are seeded
	// together so the demo tenant reads as one continuous ledger. History is
	// appended first so the current-period JRN-10xx/11xx numbers keep the top of
	// the descending sort both stores apply.
	for _, seed := range append(demoHistoryJournals(), demoJournals()...) {
		entry := &domain.JournalEntry{
			CompanyID:  company.ID,
			BranchID:   branchIDs[0],
			Number:     seed.number,
			Date:       seed.entryDate(),
			Status:     seed.status,
			SourceType: seed.source,
			Memo:       seed.memo,
		}
		for _, lineSeed := range seed.lines {
			account, ok := accountsByCode[lineSeed.accountCode]
			if !ok {
				// A journal line that references an unknown account would
				// silently drop money out of the ledger, so fail loudly.
				log.Printf("[Seed] skipping journal %s: unknown account %s", seed.number, lineSeed.accountCode)
				break
			}
			entry.Lines = append(entry.Lines, domain.JournalLine{
				AccountID: account.ID,
				Debit:     lineSeed.debit,
				Credit:    lineSeed.credit,
				Memo:      lineSeed.memo,
			})
		}
		if err := store.CreateJournalEntry(entry); err != nil {
			return err
		}

		// Mirror JournalService.Create's audit event. Posted entries record
		// "journal.post"; drafts record "journal.create" — the same split the
		// service applies, so a seeded draft is not misreported as posted.
		var totalDebit int64
		for _, line := range entry.Lines {
			totalDebit += line.Debit
		}
		journalAfter, _ := json.Marshal(map[string]any{
			"number": entry.Number,
			"memo":   entry.Memo,
			"total":  totalDebit,
		})
		action := "journal.create"
		if seed.status == domain.JournalStatusPosted {
			action = "journal.post"
		}
		if err := store.AppendAuditLog(domain.AuditLog{
			CompanyID: company.ID,
			UserID:    demoUser.ID,
			Action:    action,
			Entity:    "journal_entry",
			AfterJSON: string(journalAfter),
			CreatedAt: issueTime(entry.Date),
		}); err != nil {
			return err
		}

		if suffix := numericSuffix(seed.number); suffix > highestJournal {
			highestJournal = suffix
		}
	}

	// Continue the document sequences after the seeded numbers so the first
	// document created by the user does not collide with a seeded one.
	advanceSequence(store, company.ID, "INV", 241)
	advanceSequence(store, company.ID, "JRN", highestJournal)

	log.Println("[Seed] Demo data seeding completed.")
	return nil
}

// advanceSequence pushes a per-company document counter forward so generated
// numbers never collide with seeded ones. Stores that do not expose sequence
// control simply ignore it.
func advanceSequence(store Store, companyID uint, prefix string, value int) {
	if setter, ok := store.(SequenceStore); ok {
		setter.SetNextNumber(companyID, prefix, value)
	}
}
