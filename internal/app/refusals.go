package app

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// refusals are what the engine serves without instead of failing: an
// optional setting turned off, a declared merchant not provisioned, a
// dependency it waits for. The documents a merchant's configuration does not
// serve are the cache's (merchantdocs.Set.Rejected).
type refusals struct {
	mu    sync.Mutex
	why   map[string]string
	names map[billing.MerchantID]string
}

// Refuse records that subject is not served, and why. A new or changed
// reason is logged at Error once.
func (r *Runtime) Refuse(subject string, err error) {
	if r == nil || err == nil {
		return
	}
	f := &r.refusals
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.why == nil {
		f.why = map[string]string{}
	}
	if f.why[subject] == err.Error() {
		return
	}
	f.why[subject] = err.Error()
	log.WithField("subject", subject).WithError(err).Error("openrails: not served; everything else serves")
}

// Unrefuse clears subject: it serves again.
func (r *Runtime) Unrefuse(subject string) {
	if r == nil {
		return
	}
	f := &r.refusals
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.why[subject]; ok {
		delete(f.why, subject)
		log.WithField("subject", subject).Info("openrails: serves again")
	}
}

// RefuseDegraded records each optional setting Degrade turned off.
func (r *Runtime) RefuseDegraded(degraded []config.Degradation) {
	for _, d := range degraded {
		r.Refuse(d.Field, d.Err)
	}
}

// NameMerchant names a merchant by its slug in Refusals.
func (r *Runtime) NameMerchant(id billing.MerchantID, slug string) {
	f := &r.refusals
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == nil {
		f.names = map[billing.MerchantID]string{}
	}
	f.names[id] = slug
}

// Refusals lists, sorted, everything not served as "subject: why": each
// refusal recorded, and each document of a loaded merchant's configuration
// the merchant does not serve ("merchants.<slug>.psps/<key>").
func (r *Runtime) Refusals() []string {
	if r == nil {
		return nil
	}
	f := &r.refusals
	f.mu.Lock()
	out := make([]string, 0, len(f.why))
	for subject, why := range f.why {
		out = append(out, subject+": "+why)
	}
	names := maps.Clone(f.names)
	f.mu.Unlock()
	if r.MerchantConfig != nil {
		for id, docs := range r.MerchantConfig.Rejected() {
			name := names[id]
			if name == "" {
				name = id.String()
			}
			for path, why := range docs {
				out = append(out, "merchants."+name+"."+path+": "+why)
			}
		}
	}
	slices.Sort(out)
	return out
}

// RefusalState is nil when everything configured is served; otherwise it
// names each refusal. It never touches the network.
func (r *Runtime) RefusalState() error {
	refused := r.Refusals()
	if len(refused) == 0 {
		return nil
	}
	return errors.New("not served: " + strings.Join(refused, "; "))
}
