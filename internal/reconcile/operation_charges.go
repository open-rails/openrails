package reconcile

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// OperationReader names the OpenRails provider operation that made a remote
// charge, when the rail records it on the charge: zero when none did.
type OperationReader interface {
	OperationOf(ctx context.Context, t *RemoteTransaction) (uuid.UUID, error)
}

// OperationOf reads an NMI sale's order description, which an engine renewal
// sets to name its operation.
func (f *NMIFetcher) OperationOf(_ context.Context, t *RemoteTransaction) (uuid.UUID, error) {
	var raw struct {
		OrderDescription string `json:"order_description"`
	}
	if json.Unmarshal(t.Raw, &raw) != nil {
		return uuid.Nil, nil
	}
	id, _ := subscriptions.ParseSubscriptionCollectionDescription(raw.OrderDescription)
	return id, nil
}

// OperationOf reads the metadata of a Stripe charge's PaymentIntent, which
// every PaymentIntent OpenRails creates carries.
func (f *StripeFetcher) OperationOf(ctx context.Context, t *RemoteTransaction) (uuid.UUID, error) {
	pi := decodeBreadcrumbs(t.Raw).PaymentIntent
	if pi == "" {
		return uuid.Nil, nil
	}
	body, err := f.get(ctx, "/v1/payment_intents/"+url.PathEscape(pi))
	if err != nil {
		return uuid.Nil, err
	}
	var intent struct {
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(body, &intent); err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(strings.TrimSpace(intent.Metadata[subscriptions.StripeOperationMetadata]))
	if err != nil {
		return uuid.Nil, nil
	}
	return id, nil
}

// deferOperationCharges drops the missing-charge findings for charges an
// OpenRails provider operation is executing right now (claimed, its lease
// live): the operation records the charge it is making, so the charge is not
// missing but still being recorded. A charge whose operation is not executing
// (awaiting verification, ended without recording it) stays a finding.
func (e *Engine) deferOperationCharges(ctx context.Context, provider Provider, pspID uuid.UUID, fetcher RailFetcher, snap *RemoteSnapshot, findings []Finding) []Finding {
	if k, ok := fetcher.(keyedFetcher); ok {
		fetcher = k.RailFetcher
	}
	reader, ok := fetcher.(OperationReader)
	if !ok {
		return findings
	}
	charges := map[string]*RemoteTransaction{}
	for i := range snap.Transactions {
		if t := &snap.Transactions[i]; t.Type == TransactionTypeSale && t.TransactionID != "" {
			charges[t.TransactionID] = t
		}
	}
	var candidates []string
	for _, f := range findings {
		if f.Type == FindingChargeMissingLocal && charges[f.SubjectKey] != nil {
			candidates = append(candidates, f.SubjectKey)
		}
	}
	if len(candidates) == 0 {
		return findings
	}
	now := e.now()
	if busy, err := e.Local.ExecutingOperations(ctx, pspID, nil, now); err != nil || !busy[uuid.Nil] {
		return findings
	}
	operations := map[string]uuid.UUID{}
	var ids []uuid.UUID
	for _, charge := range candidates {
		id, err := reader.OperationOf(ctx, charges[charge])
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{"provider": provider, "transaction_id": charge}).
				Warn("reconcile: the operation behind a charge is unreadable; the charge stays a finding")
			continue
		}
		if id != uuid.Nil {
			operations[charge] = id
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return findings
	}
	executing, err := e.Local.ExecutingOperations(ctx, pspID, ids, now)
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("provider", provider).Warn("reconcile: executing operations unreadable; charges stay findings")
		return findings
	}
	out := findings[:0]
	for _, f := range findings {
		if f.Type == FindingChargeMissingLocal && executing[operations[f.SubjectKey]] && operations[f.SubjectKey] != uuid.Nil {
			log.WithContext(ctx).WithFields(log.Fields{"provider": provider, "transaction_id": f.SubjectKey, "operation_id": operations[f.SubjectKey]}).
				Info("reconcile: charge left to the operation executing it")
			continue
		}
		out = append(out, f)
	}
	return out
}
