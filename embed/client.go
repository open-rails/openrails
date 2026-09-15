package embed

import (
	"fmt"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ClientOption configures the shared client returned by Runtime.Client.
type ClientOption func(*clientOptions)

type clientOptions struct {
	currency      string
	remoteOptions []openrails.RemoteOption
}

func WithCurrency(currency string) ClientOption {
	return func(c *clientOptions) { c.currency = strings.TrimSpace(currency) }
}

// WithRemoteOptions applies the same call options as NewRemote.
func WithRemoteOptions(options ...openrails.RemoteOption) ClientOption {
	return func(c *clientOptions) { c.remoteOptions = append(c.remoteOptions, options...) }
}

func merchantMismatchMsg(bound, pinned merchant.ID) string {
	return fmt.Sprintf("openrails: call pinned to merchant %s but client is bound to merchant %s", pinned, bound)
}
