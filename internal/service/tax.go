package service

// TaxService calculates the illustrative output-VAT figure used by invoices
// and the tax summary. The release applies the general 11% rate.
type TaxService struct {
	ppnRatePercent int64
}

func NewTaxService() *TaxService {
	return &TaxService{ppnRatePercent: 11}
}

// PPNResult is the output of a VAT calculation over a taxable base (DPP).
type PPNResult struct {
	DPP       int64 `json:"dpp"`
	TaxAmount int64 `json:"taxAmount"`
	Total     int64 `json:"total"`
}

// CalculatePPN computes VAT from a taxable base using the configured rate.
func (t *TaxService) CalculatePPN(dpp int64) PPNResult {
	tax := round(dpp*int64(t.ppnRatePercent), 100)
	return PPNResult{DPP: dpp, TaxAmount: tax, Total: dpp + tax}
}

// Rate returns the effective PPN rate percentage (e.g. 11).
func (t *TaxService) Rate() int64 {
	return t.ppnRatePercent
}

// round divides n by d with half-up rounding to the nearest integer.
func round(n, d int64) int64 {
	if d == 0 {
		return n
	}
	return (n + d/2) / d
}
