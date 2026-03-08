package handlers

type IRequest interface {
	Path() any
	Query() any
	Body() any
	SetDefaults()
}

// BaseRequest provides default nil implementations for Request interface methods.
type BaseRequest struct{}

// Path returns nil by default, indicating no URI parameters.
func (b *BaseRequest) Path() any {
	return nil
}

// Query returns nil by default, indicating no query parameters.
func (b *BaseRequest) Query() any {
	return nil
}

// Body returns nil by default, indicating no body parameters.
func (b *BaseRequest) Body() any {
	return nil
}

func (b *BaseRequest) SetDefaults() {
}

// PaginationParams provides common pagination parameters that can be embedded in request structs
// Uses limit/offset pattern (Stripe-style) for better SQL mapping and flexibility
type PaginationParams struct {
	Limit  int `form:"limit" default:"20" validate:"min=1,max=100"`
	Offset int `form:"offset" default:"0" validate:"min=0"`
}

// SetPaginationDefaults sets default values for pagination parameters
// This method accepts a custom default limit to allow flexibility across different endpoints
func (p *PaginationParams) SetPaginationDefaults(defaultLimit int) {
	p.Limit = defaultLimit
	p.Offset = 0
}

// CapLimit ensures Limit doesn't exceed maxLimit and returns true if it was capped
func (p *PaginationParams) CapLimit(maxLimit int) bool {
	if maxLimit < p.Limit {
		p.Limit = maxLimit
		return true
	}
	return false
}

// -------------------------------- GetSubscription Request --------------------------------

type GetSubscriptionRequest struct {
	BaseRequest
}

// -------------------------------- CancelSubscription Request --------------------------------

type CancelSubscriptionBodyParams struct {
	Feedback string `json:"feedback" validate:"max=500"`
}

type CancelSubscriptionRequest struct {
	BaseRequest
	CancelSubscriptionBodyParams
}

func (r *CancelSubscriptionRequest) Body() any {
	return &r.CancelSubscriptionBodyParams
}

// -------------------------------- Payment Methods Requests --------------------------------

type ListPaymentMethodsQueryParams struct {
	PaginationParams
	IncludeInactive bool `form:"include_inactive"`
}

type ListPaymentMethodsRequest struct {
	BaseRequest
	ListPaymentMethodsQueryParams
}

func (r *ListPaymentMethodsRequest) Query() any {
	return &r.ListPaymentMethodsQueryParams
}

func (r *ListPaymentMethodsRequest) SetDefaults() {
	r.SetPaginationDefaults(20)
}

type DeletePaymentMethodPathParams struct {
	ID string `uri:"id" binding:"required"`
}

type DeletePaymentMethodRequest struct {
	BaseRequest
	DeletePaymentMethodPathParams
}

func (r *DeletePaymentMethodRequest) Path() any {
	return &r.DeletePaymentMethodPathParams
}

type CreatePaymentMethodRequest struct {
	PaymentToken string `json:"payment_token" binding:"required"`
	FirstName    string `json:"first_name" binding:"required"`
	LastName     string `json:"last_name" binding:"required"`
	Address1     string `json:"address1" binding:"required"`
	City         string `json:"city" binding:"required"`
	State        string `json:"state" binding:"omitempty"`
	Zip          string `json:"zip" binding:"required"`
	Country      string `json:"country" binding:"required"`
	Phone        string `json:"phone" binding:"omitempty"`
	Email        string `json:"email" binding:"omitempty,email"`
	Company      string `json:"company" binding:"omitempty"`
	Address2     string `json:"address2" binding:"omitempty"`
	Provider     string `json:"provider" binding:"omitempty"`
	LastFour     string `json:"last_four" binding:"omitempty"`
	CardType     string `json:"card_type" binding:"omitempty"`
	ExpiryDate   string `json:"expiry_date" binding:"omitempty"`
}

type UpdatePaymentMethodPathParams struct {
	ID string `uri:"id" binding:"required"`
}

type UpdatePaymentMethodBodyParams struct {
	PaymentToken string  `json:"payment_token" binding:"required"`
	FirstName    *string `json:"first_name"`
	LastName     *string `json:"last_name"`
	Address1     *string `json:"address1"`
	City         *string `json:"city"`
	State        *string `json:"state"`
	Zip          *string `json:"zip"`
	Country      *string `json:"country"`
	Phone        *string `json:"phone"`
	Email        *string `json:"email" binding:"omitempty,email"`
	Company      *string `json:"company"`
	Address2     *string `json:"address2"`
	Provider     *string `json:"provider"`
}

type UpdatePaymentMethodRequest struct {
	BaseRequest
	UpdatePaymentMethodPathParams
	UpdatePaymentMethodBodyParams
}

func (r *UpdatePaymentMethodRequest) Path() any {
	return &r.UpdatePaymentMethodPathParams
}

func (r *UpdatePaymentMethodRequest) Body() any {
	return &r.UpdatePaymentMethodBodyParams
}

// -------------------------------- Checkout Session Requests --------------------------------

type CheckoutSessionPaymentParams struct {
	Processor       string `json:"processor" binding:"required,oneof=mobius ccbill solana stripe"`
	PaymentMethodID string `json:"payment_method_id,omitempty" binding:"omitempty"`
	PaymentToken    string `json:"payment_token,omitempty"`

	// Solana-specific fields
	TokenSymbol string `json:"token_symbol,omitempty" binding:"omitempty"`
	Flow        string `json:"flow,omitempty" binding:"omitempty,oneof=transfer_request transaction_request"`
	Wallet      string `json:"wallet,omitempty" binding:"omitempty"`

	// CCBill/Stripe billing fields (flattened)
	Email     string `json:"email,omitempty" binding:"omitempty,email"`
	FirstName string `json:"first_name,omitempty" binding:"omitempty,max=100"`
	LastName  string `json:"last_name,omitempty" binding:"omitempty,max=100"`
	Address1  string `json:"address1,omitempty" binding:"omitempty,max=200"`
	City      string `json:"city,omitempty" binding:"omitempty,max=100"`
	State     string `json:"state,omitempty" binding:"omitempty,max=50"`
	Zip       string `json:"zip,omitempty" binding:"omitempty,max=20"`
	Country   string `json:"country,omitempty" binding:"omitempty,max=2"`

	LastFour   string `json:"last_four,omitempty" binding:"omitempty"`
	CardType   string `json:"card_type,omitempty" binding:"omitempty"`
	ExpiryDate string `json:"expiry_date,omitempty" binding:"omitempty"`
}

type CheckoutSessionCreateBodyParams struct {
	PriceID  string                       `json:"price_id" binding:"required"`
	Mode     string                       `json:"mode,omitempty" binding:"omitempty,oneof=one_off subscription"`
	Payment  CheckoutSessionPaymentParams `json:"payment" binding:"required"`
	Metadata map[string]string            `json:"metadata,omitempty"`
}

type CheckoutSessionCreateRequest struct {
	BaseRequest
	CheckoutSessionCreateBodyParams
	IdempotencyKey string `json:"-"`
}

func (r *CheckoutSessionCreateRequest) Body() any {
	return &r.CheckoutSessionCreateBodyParams
}

type CheckoutSessionConfirmBodyParams struct {
	Payment struct {
		Processor string `json:"processor" binding:"required,oneof=solana"`
		Signature string `json:"signature,omitempty"`
		Wallet    string `json:"wallet,omitempty"`
	} `json:"payment" binding:"required"`
}

type CheckoutSessionConfirmRequest struct {
	BaseRequest
	CheckoutSessionConfirmBodyParams
}

func (r *CheckoutSessionConfirmRequest) Body() any {
	return &r.CheckoutSessionConfirmBodyParams
}

// Note: UpdateSubscriptionPaymentMethod uses a local struct in the handler
// with subscription ID from path param and payment_method_id in body.
