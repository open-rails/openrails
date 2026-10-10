package merchantdocs

import (
	"context"
	"sync"

	"github.com/open-rails/openrails/billing"
)

// Source is where merchant configuration lives: a file or Vault.
type Source interface {
	// Load reads every document of the merchant; a merchant with none
	// answers an empty Set.
	Load(ctx context.Context, id billing.MerchantID) (Set, error)
	// Writable reports whether the Put methods change the configuration.
	Writable() bool
	// PutMerchant, PutPSP and PutCustodian write one document when cas is its
	// current revision (0: a new document), answering ErrRevisionMismatch
	// otherwise.
	PutMerchant(ctx context.Context, id billing.MerchantID, doc Merchant, cas int64) (Doc[Merchant], error)
	PutPSP(ctx context.Context, id billing.MerchantID, key string, doc PSP, cas int64) (Doc[PSP], error)
	PutCustodian(ctx context.Context, id billing.MerchantID, key string, doc Custodian, cas int64) (Doc[Custodian], error)
	// Delete removes every document of the merchant.
	Delete(ctx context.Context, id billing.MerchantID) error
}

// FileSource holds configuration read from files: in memory, read-only.
type FileSource struct {
	mu   sync.RWMutex
	sets map[billing.MerchantID]Set
}

// NewFileSource returns an empty file source; boot fills it with Put.
func NewFileSource() *FileSource {
	return &FileSource{sets: map[billing.MerchantID]Set{}}
}

// Put records the merchant's configuration as its file declares it.
func (f *FileSource) Put(id billing.MerchantID, set Set) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets[id] = set.Clone()
}

// Declared reports whether a file declares the merchant.
func (f *FileSource) Declared(id billing.MerchantID) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.sets[id]
	return ok
}

func (f *FileSource) Load(_ context.Context, id billing.MerchantID) (Set, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.sets[id].Clone(), nil
}

func (f *FileSource) Writable() bool { return false }

func (f *FileSource) PutMerchant(context.Context, billing.MerchantID, Merchant, int64) (Doc[Merchant], error) {
	return Doc[Merchant]{}, ErrReadOnly
}

func (f *FileSource) PutPSP(context.Context, billing.MerchantID, string, PSP, int64) (Doc[PSP], error) {
	return Doc[PSP]{}, ErrReadOnly
}

func (f *FileSource) PutCustodian(context.Context, billing.MerchantID, string, Custodian, int64) (Doc[Custodian], error) {
	return Doc[Custodian]{}, ErrReadOnly
}

// Delete holds nothing to delete: the file stays the operator's.
func (f *FileSource) Delete(_ context.Context, id billing.MerchantID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sets, id)
	return nil
}
