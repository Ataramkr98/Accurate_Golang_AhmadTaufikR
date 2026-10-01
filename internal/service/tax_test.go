package service

import "testing"

func TestCalculatePPN_ElevenPercent(t *testing.T) {
	tax := NewTaxService()
	got := tax.CalculatePPN(10_000_000)
	if got.TaxAmount != 1_100_000 {
		t.Errorf("tax = %d, want 1_100_000", got.TaxAmount)
	}
	if got.Total != 11_100_000 {
		t.Errorf("total = %d, want 11_100_000", got.Total)
	}
}

func TestCalculatePPN_RoundsToNearestRupiah(t *testing.T) {
	tax := NewTaxService()
	got := tax.CalculatePPN(9_999_999)
	if got.TaxAmount != 1_100_000 {
		t.Errorf("tax = %d, want 1_100_000 (rounded)", got.TaxAmount)
	}
}
