package lsp

import (
	"github.com/infracost/go-proto/pkg/rat"

	"github.com/infracost/lsp/internal/scanner"
)

func (s *Server) currency() string {
	s.mu.RLock()
	currency := s.settings.Currency
	s.mu.RUnlock()

	if currency == "" && s.scanner != nil {
		currency = s.scanner.CurrencyOrDefault()
	}
	currency = scanner.NormalizeCurrency(currency)
	if currency == "" {
		return "USD"
	}
	return currency
}

func (s *Server) formatCost(cost *rat.Rat) string {
	return scanner.FormatCostCurrency(cost, s.currency())
}
