package service

import (
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/services"
)

func (s *Service) creditsService() *services.CreditsService {
	return s.rt.CreditsService
}

func (s *Service) entitlementService() *services.EntitlementService {
	return s.rt.EntitlementService
}

func (s *Service) requireCreditTypeService() (*services.CreditTypeService, error) {
	if s.rt.CreditTypeService == nil {
		return nil, fmt.Errorf("billing service: credit type service unavailable")
	}
	return s.rt.CreditTypeService, nil
}

func (s *Service) requireProductService() (*services.ProductService, error) {
	if s.rt.ProductService == nil {
		return nil, fmt.Errorf("billing service: product service unavailable")
	}
	return s.rt.ProductService, nil
}

func (s *Service) requireCatalogServices() (*services.ProductService, *services.PriceService, error) {
	if s.rt.ProductService == nil || s.rt.PriceService == nil {
		return nil, nil, fmt.Errorf("billing service: price/product service unavailable")
	}
	return s.rt.ProductService, s.rt.PriceService, nil
}

func (s *Service) requirePriceService() (*services.PriceService, error) {
	if s.rt.PriceService == nil {
		return nil, fmt.Errorf("billing service: price service unavailable")
	}
	return s.rt.PriceService, nil
}

func (s *Service) requirePublicSubscriptionService() (*services.PublicSubscriptionService, error) {
	if s.rt.PublicSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.PublicSubscriptionService, nil
}

func (s *Service) requireCheckoutSessionService() (*services.CheckoutSessionService, error) {
	if s.rt.CheckoutSessionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.CheckoutSessionService, nil
}

func (s *Service) requireUserSubscriptionService() (*services.UserSubscriptionService, error) {
	if s.rt.UserSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.UserSubscriptionService, nil
}

func (s *Service) requireSubscriptionAndPaymentMethodServices() (*services.SubscriptionService, *services.PaymentMethodService, error) {
	if s.rt.SubscriptionService == nil || s.rt.PaymentMethodService == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.SubscriptionService, s.rt.PaymentMethodService, nil
}

func (s *Service) requirePaymentMethodService() (*services.PaymentMethodService, error) {
	if s.rt.PaymentMethodService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.PaymentMethodService, nil
}

func (s *Service) requireVaultService() (*services.VaultService, error) {
	if s.rt.VaultService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.VaultService, nil
}

func (s *Service) requireVaultAndPaymentMethodServices() (*services.VaultService, *services.PaymentMethodService, error) {
	if s.rt.VaultService == nil || s.rt.PaymentMethodService == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.VaultService, s.rt.PaymentMethodService, nil
}

func (s *Service) requireDB() (*db.DB, error) {
	if s.rt.DB == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.DB, nil
}

func (s *Service) requireConfig() (*config.Config, error) {
	if s.rt.Config == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.Config, nil
}

func (s *Service) requireProcessorCustomerAndConfig() (*services.ProcessorCustomerService, *config.Config, error) {
	if s.rt.ProcessorCustomerService == nil || s.rt.Config == nil {
		return nil, nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.ProcessorCustomerService, s.rt.Config, nil
}

func (s *Service) requireAdminSubscriptionService() (*services.AdminSubscriptionService, error) {
	if s.rt.AdminSubscriptionService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.AdminSubscriptionService, nil
}

func (s *Service) requirePaymentService() (*services.PaymentService, error) {
	if s.rt.PaymentService == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.PaymentService, nil
}

func (s *Service) requireAdminMetricsConfig() (*config.Config, error) {
	if s.rt.Config == nil {
		return nil, fmt.Errorf("billing service: not initialized")
	}
	return s.rt.Config, nil
}

func (s *Service) requireWebhookRuntime() (*app.Runtime, error) {
	if s.rt.RiverProducer == nil {
		return nil, fmt.Errorf("job queue unavailable")
	}
	return s.rt, nil
}
