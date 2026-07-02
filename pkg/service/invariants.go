package service

import (
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func (s *Service) moneyService() *money.MoneyService {
	if s == nil || s.rt == nil {
		return nil
	}
	return s.rt.MoneyService
}

func (s *Service) entitlementService() *entitlements.EntitlementService {
	if s == nil || s.rt == nil {
		return nil
	}
	return s.rt.EntitlementService
}

func (s *Service) runtime() (*app.Runtime, error) {
	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt, nil
}

func (s *Service) requireProductService() (*catalog.ProductService, error) {
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
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.PriceService == nil {
		return nil, fmt.Errorf("billing service: price service unavailable")
	}
	return rt.PriceService, nil
}

func (s *Service) requirePublicSubscriptionService() (*catalog.PublicSubscriptionService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.PublicSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.PublicSubscriptionService, nil
}

func (s *Service) requireCheckoutSessionService() (*checkout.CheckoutSessionService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.CheckoutSessionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.CheckoutSessionService, nil
}

func (s *Service) requireUserSubscriptionService() (*subscriptions.UserSubscriptionService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.UserSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.UserSubscriptionService, nil
}

func (s *Service) requireSubscriptionAndPaymentMethodServices() (*subscriptions.SubscriptionService, *paymentmethods.PaymentMethodService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, nil, err
	}
	if rt.SubscriptionService == nil || rt.PaymentMethodService == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.SubscriptionService, rt.PaymentMethodService, nil
}

func (s *Service) requirePaymentMethodService() (*paymentmethods.PaymentMethodService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.PaymentMethodService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.PaymentMethodService, nil
}

func (s *Service) requireVaultService() (*paymentmethods.VaultService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.VaultService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.VaultService, nil
}

func (s *Service) requireVaultAndPaymentMethodServices() (*paymentmethods.VaultService, *paymentmethods.PaymentMethodService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, nil, err
	}
	if rt.VaultService == nil || rt.PaymentMethodService == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.VaultService, rt.PaymentMethodService, nil
}

func (s *Service) requireDB() (*db.DB, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.DB == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.DB, nil
}

func (s *Service) requireConfig() (*config.Config, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Config == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.Config, nil
}

func (s *Service) requireRailCustomerAndConfig() (*payments.RailCustomerService, *config.Config, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, nil, err
	}
	if rt.RailCustomerService == nil || rt.Config == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.RailCustomerService, rt.Config, nil
}

func (s *Service) requireAdminSubscriptionService() (*subscriptions.AdminSubscriptionService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.AdminSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.AdminSubscriptionService, nil
}

func (s *Service) requirePaymentService() (*payments.PaymentService, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.PaymentService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.PaymentService, nil
}

func (s *Service) requireAdminMetricsConfig() (*config.Config, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Config == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return rt.Config, nil
}

func (s *Service) requireWebhookRuntime() (*app.Runtime, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.RiverProducer == nil {
		return nil, fmt.Errorf("job queue unavailable")
	}
	return rt, nil
}
