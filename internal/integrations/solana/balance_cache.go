package solana

import (
	"context"
	"sync"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/providerposture"
)

const BalanceCacheTTL = 5 * time.Minute

type balanceKey struct {
	provider providerposture.Key
	wallet   solanago.PublicKey
}
type balanceSample struct {
	lamports uint64
	expires  time.Time
}

// BalanceCache shares successful, network-verified reads between the optional
// health probe and operator alert. Failed reads never reuse an expired success.
type BalanceCache struct {
	mu      sync.Mutex
	samples map[balanceKey]balanceSample
}

func (c *BalanceCache) Read(ctx context.Context, rpc *RPCClient, merchantID, pspID uuid.UUID, wallet solanago.PublicKey, now time.Time) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	provider := rpc.fallback.PostureKey()
	provider.MerchantID, provider.PSPID = merchantID, pspID
	key := balanceKey{provider: provider, wallet: wallet}
	c.mu.Lock()
	sample, ok := c.samples[key]
	for k, v := range c.samples {
		if !now.Before(v.expires) {
			delete(c.samples, k)
		}
	}
	c.mu.Unlock()
	if ok && now.Before(sample.expires) {
		return sample.lamports, nil
	}
	// Network checks and RPC reads run outside the cache lock so a stalled
	// merchant cannot block other callers or prevent cancellation.
	if _, err := rpc.CheckPosture(ctx); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	balance, err := rpc.GetBalance(ctx, wallet)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	if c.samples == nil {
		c.samples = make(map[balanceKey]balanceSample)
	}
	// An earlier concurrent read must not replace a more recent sample.
	if current := c.samples[key]; !current.expires.After(now.Add(BalanceCacheTTL)) {
		c.samples[key] = balanceSample{lamports: balance, expires: now.Add(BalanceCacheTTL)}
	}
	c.mu.Unlock()
	return balance, nil
}
