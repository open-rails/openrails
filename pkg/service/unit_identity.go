package service

import "context"

func (s *Service) resolveCurrency(_ context.Context, raw string) (string, error) {
	return requireCurrency(raw)
}

// DisplayCurrency returns a validated canonical currency code.
func (s *Service) DisplayCurrency(_ context.Context, code string) (string, error) {
	return requireCurrency(code)
}
