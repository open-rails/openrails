package recurring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

// ErrNoCranker: the deployment lists no Solana cranker, so Solana rebilling
// is off. One-time Solana payments still work.
var ErrNoCranker = errors.New("solana rebilling is off: this deployment lists no solana.crankers")

// ErrUnheldPlan: no listed cranker holds the key that owns the plan, so its
// renewals cannot be pulled.
var ErrUnheldPlan = errors.New("no listed Solana cranker owns the plan, so its renewals cannot be pulled")

// CrankerKeyChangedError refuses a cranker whose key is no longer the one it
// was pinned to on first use.
type CrankerKeyChangedError struct {
	Name        string
	Pinned, Now solanago.PublicKey
}

func (e *CrankerKeyChangedError) Error() string {
	return fmt.Sprintf("solana cranker %s now has public key %s, not %s, the key it was pinned to: a Transit key rotation would make it another cranker; restore that key, or list a new Transit key as another cranker", e.Name, e.Now, e.Pinned)
}

// Pins record each cranker's public key on first use.
type Pins interface {
	// Pin records key for the named cranker unless it has one, and answers
	// the key it is pinned to.
	Pin(ctx context.Context, name string, key solanago.PublicKey) (solanago.PublicKey, error)
}

// Cranker is one listed cranker.
type Cranker struct {
	// Name names it without its secret (config.SolanaCrankerConfig.Name).
	Name   string
	Signer solanaint.Signer
}

// Crankers are the deployment's crankers in order, each pinned to its first
// public key. The first publishes new plans; each pulls the plans it owns.
type Crankers struct {
	list []*pinnedCranker
}

// NewCrankers lists crankers, pinning each in pins (nil pins nothing).
func NewCrankers(pins Pins, crankers ...Cranker) *Crankers {
	c := &Crankers{}
	for _, k := range crankers {
		c.list = append(c.list, &pinnedCranker{name: k.Name, signer: k.Signer, pins: pins})
	}
	return c
}

// BuildCrankers builds the configured crankers; none when none is listed.
func BuildCrankers(cfgs []config.SolanaCrankerConfig, transit solanaint.TransitClient, pins Pins) (*Crankers, error) {
	crankers := make([]Cranker, 0, len(cfgs))
	for i, cfg := range cfgs {
		var signer solanaint.Signer
		switch strings.TrimSpace(cfg.Mode) {
		case config.CrankerVaultTransit:
			if transit == nil {
				return nil, fmt.Errorf("solana.crankers[%d]: vault_transit needs Vault Transit", i)
			}
			signer = solanaint.NewTransitSigner(transit, strings.TrimSpace(cfg.Key), 0)
		case config.CrankerLocalKeypair:
			key, err := solanago.PrivateKeyFromBase58(strings.TrimSpace(cfg.PrivateKey))
			if err != nil {
				return nil, fmt.Errorf("solana.crankers[%d].private_key is not a base58 Solana keypair", i)
			}
			signer = solanaint.NewKeypairSigner(key)
		default:
			return nil, fmt.Errorf("solana.crankers[%d].mode must be %s or %s", i, config.CrankerVaultTransit, config.CrankerLocalKeypair)
		}
		crankers = append(crankers, Cranker{Name: cfg.Name(), Signer: signer})
	}
	return NewCrankers(pins, crankers...), nil
}

// Len is how many crankers are listed.
func (c *Crankers) Len() int {
	if c == nil {
		return 0
	}
	return len(c.list)
}

// Publisher is the first cranker: the one new plans are published by.
func (c *Crankers) Publisher(ctx context.Context) (solanaint.Signer, solanago.PublicKey, error) {
	if c.Len() == 0 {
		return nil, solanago.PublicKey{}, ErrNoCranker
	}
	key, err := c.list[0].PublicKey(ctx)
	if err != nil {
		return nil, solanago.PublicKey{}, err
	}
	return c.list[0], key, nil
}

// Owner is the listed cranker holding owner's key. ErrUnheldPlan when every
// cranker answered and none does; a cranker that cannot answer is reported
// instead, since it may be the owner.
func (c *Crankers) Owner(ctx context.Context, owner solanago.PublicKey) (solanaint.Signer, error) {
	if c.Len() == 0 {
		return nil, ErrNoCranker
	}
	var errs []error
	for _, k := range c.list {
		key, err := k.PublicKey(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if key.Equals(owner) {
			return k, nil
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, fmt.Errorf("%w (owner %s)", ErrUnheldPlan, owner)
}

// Keys are the keys of the crankers that answer, with what the others said.
func (c *Crankers) Keys(ctx context.Context) ([]solanago.PublicKey, error) {
	var keys []solanago.PublicKey
	var errs []error
	for _, k := range c.list {
		key, err := k.PublicKey(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		keys = append(keys, key)
	}
	return keys, errors.Join(errs...)
}

// Changed names each listed cranker refused because its key is no longer
// the one it was pinned to; the others map to nil.
func (c *Crankers) Changed(ctx context.Context) map[string]error {
	out := map[string]error{}
	if c == nil {
		return out
	}
	for _, k := range c.list {
		var changed *CrankerKeyChangedError
		if _, err := k.PublicKey(ctx); errors.As(err, &changed) {
			out[k.name] = changed
		} else {
			out[k.name] = nil
		}
	}
	return out
}

// pinnedCranker is a cranker that refuses to sign once its key is no longer
// the one pinned on its first use.
type pinnedCranker struct {
	name   string
	signer solanaint.Signer
	pins   Pins

	mu     sync.Mutex
	pinned solanago.PublicKey
}

func (p *pinnedCranker) PublicKey(ctx context.Context) (solanago.PublicKey, error) {
	key, err := p.signer.PublicKey(ctx)
	if err != nil {
		return solanago.PublicKey{}, fmt.Errorf("solana cranker %s: %w", p.name, err)
	}
	p.mu.Lock()
	pinned := p.pinned
	p.mu.Unlock()
	if pinned.IsZero() {
		pinned = key
		if p.pins != nil {
			if pinned, err = p.pins.Pin(ctx, p.name, key); err != nil {
				return solanago.PublicKey{}, fmt.Errorf("solana cranker %s: pin its key: %w", p.name, err)
			}
		}
		p.mu.Lock()
		p.pinned = pinned
		p.mu.Unlock()
	}
	if !key.Equals(pinned) {
		return solanago.PublicKey{}, &CrankerKeyChangedError{Name: p.name, Pinned: pinned, Now: key}
	}
	return key, nil
}

func (p *pinnedCranker) SignMessage(ctx context.Context, message []byte) (solanago.Signature, error) {
	if _, err := p.PublicKey(ctx); err != nil {
		return solanago.Signature{}, err
	}
	return p.signer.SignMessage(ctx, message)
}

// Treasury is the address the merchant's Solana PSP is paid into: every
// plan's only destination.
type Treasury func(ctx context.Context, merchantID billing.MerchantID) (solanago.PublicKey, error)

// planPaysInto refuses a plan whose destinations do not name treasury. A plan
// naming none (published before treasuries) pays wherever its owner pulls.
func planPaysInto(plan solanago.PublicKey, destinations [4]solanago.PublicKey, treasury solanago.PublicKey) error {
	named := false
	for _, d := range destinations {
		if d.IsZero() {
			continue
		}
		if d.Equals(treasury) {
			return nil
		}
		named = true
	}
	if !named {
		return nil
	}
	return fmt.Errorf("recurring: plan %s pays into %s, not the merchant's treasury %s; a plan's destination is immutable, so a new treasury takes new prices", plan, destinations[0], treasury)
}
