package api

import (
	"testing"
	"time"

	"copytrade/internal/model"
)

func TestSummarizeCopies(t *testing.T) {
	at := time.Now()
	cs := []model.NonsolCopy{
		{Status: model.NonsolClosed, SolUSD: 100, SolIn: 0.1, FeesSol: 0.001, SolOut: 0.151}, // +$5
		{Status: model.NonsolClosed, SolUSD: 100, SolIn: 0.1, FeesSol: 0.001, SolOut: 0.081}, // -$2
		{Status: model.NonsolOpen, SolUSD: 100, SolIn: 0.1, FeesSol: 0.0005, MarkSol: 0.1105, MarkedAt: &at},
		{Status: model.NonsolOpen, SolUSD: 100, SolIn: 0.1},
		{Status: model.NonsolSkipped, SkipReason: "TOKEN_NOT_TRADABLE"},
		{Status: model.NonsolSkipped, SkipReason: "TOKEN_NOT_TRADABLE"},
	}
	var rows []copyRow
	for _, c := range cs {
		rows = append(rows, toCopyRow(c))
	}
	s := summarizeCopies(rows)
	if s.Copies != 2 || s.Won != 1 || s.TotalUSD != 3 || *s.BestUSD != 5 || *s.WithoutBestUSD != -2 || *s.WinRate != 50 {
		t.Fatalf("bad summary %+v", s)
	}
	if s.Open != 2 || s.OpenUSD != 1 || s.Skipped != 2 || s.SkippedByReason["TOKEN_NOT_TRADABLE"] != 2 {
		t.Fatalf("bad open/skip %+v", s)
	}
	if rows[0].Pct != 50 {
		t.Fatalf("pct %v", rows[0].Pct)
	}
}
