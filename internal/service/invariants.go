package service

import (
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payment"
)

func (s *Service) moneyService() *money.MoneyService {
	if s == nil || s.rt == nil {
		return nil
	}
	return s.rt.MoneyService
}

func (s *Service) runtime() (*app.Runtime, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt, nil
}

func (s *Service) requireProductService() (*catalog.ProductService, error) {
	if s != nil && s.catalogTx != nil {
		return catalog.NewProductService(s.catalogTx), nil
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.ProductService == nil {
		return nil, fmt.Errorf("billing service: product service unavailable")
	}
	return rt.ProductService, nil
}

func (s *Service) requireCatalogServices() (*catalog.ProductService, *catalog.PriceService, error) {
	if s != nil && s.catalogTx != nil {
		return catalog.NewProductService(s.catalogTx), catalog.NewPriceService(s.catalogTx), nil
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, nil, err
	}
	if rt.ProductService == nil || rt.PriceService == nil {
		return nil, nil, fmt.Errorf("billing service: price/product service unavailable")
	}
	return rt.ProductService, rt.PriceService, nil
}

func (s *Service) requirePriceService() (*catalog.PriceService, error) {
	if s != nil && s.catalogTx != nil {
		return catalog.NewPriceService(s.catalogTx), nil
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.PriceService == nil {
		return nil, fmt.Errorf("billing service: price service unavailable")
	}
	return rt.PriceService, nil
}

func (s *Service) requireInvoiceAttemptService() (*payment.InvoiceAttemptService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.InvoiceAttemptService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.InvoiceAttemptService, nil
}

func (s *Service) requireDB() (*db.DB, error) {
	if s != nil && s.catalogTx != nil {
		return s.catalogTx, nil
	}
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.DB, nil
}
