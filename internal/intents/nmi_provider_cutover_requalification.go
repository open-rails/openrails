package intents

import (
	"github.com/open-rails/openrails/pkg/merchant"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/providerqualification"
)

// Account continuity is explicitly attested by an operator. Provider object
// IDs corroborate the frozen address; they do not prove account continuity.
// This history is outside nmiCutoverProgress so ordinary, possibly stale
// progress updates cannot overwrite it.
type cutoverAccountRequalification struct {
	Role                  string                       `json:"role"`
	Qualification         providerqualification.Record `json:"qualification"`
	OriginalFingerprint   string                       `json:"original_fingerprint"`
	PreviousFingerprint   string                       `json:"previous_fingerprint"`
	CredentialFingerprint string                       `json:"credential_fingerprint"`
	CredentialVersion     int                          `json:"credential_version"`
	Actor                 string                       `json:"actor"`
	Reason                string                       `json:"reason"`
	RecordedAt            time.Time                    `json:"recorded_at"`
}

type cutoverAccountBinding struct {
	Qualification       providerqualification.Record
	OriginalFingerprint string
	Fingerprint         string
	Version             int
	Revision            int
}

func cutoverAccountBindings(in gen.OpenrailsRailIntent, p nmiCutoverPayload) (map[string]cutoverAccountBinding, []cutoverAccountRequalification, error) {
	bindings := map[string]cutoverAccountBinding{
		"source": {Qualification: p.SourceQualification.Record, OriginalFingerprint: p.SourceCredentialFingerprint, Fingerprint: p.SourceCredentialFingerprint},
		"target": {Qualification: p.TargetQualification.Record, OriginalFingerprint: p.TargetCredentialFingerprint, Fingerprint: p.TargetCredentialFingerprint},
	}
	if p.SourceQualification.PSPID != p.Request.ExpectedSourcePSPID || p.TargetQualification.PSPID != p.Request.ExpectedTargetPSPID || p.SourceQualification.Environment != p.TargetQualification.Environment || p.SourceQualification.Contract != providerqualification.NMIContract || p.TargetQualification.Contract != providerqualification.NMIContract {
		return nil, nil, ErrResolutionRejected
	}
	if p.SourceQualification.CredentialVersion == nil || p.TargetQualification.CredentialVersion == nil || p.SourceQualification.CredentialFingerprint != p.SourceCredentialFingerprint || p.TargetQualification.CredentialFingerprint != p.TargetCredentialFingerprint || !providerqualification.ValidFingerprint(p.SourceCredentialFingerprint) || !providerqualification.ValidFingerprint(p.TargetCredentialFingerprint) {
		return nil, nil, ErrResolutionRejected
	}
	source, target := bindings["source"], bindings["target"]
	source.Version = *p.SourceQualification.CredentialVersion
	target.Version = *p.TargetQualification.CredentialVersion
	bindings["source"], bindings["target"] = source, target
	var document map[string]json.RawMessage
	if len(in.ResultEvidence) > 0 {
		if err := json.Unmarshal(in.ResultEvidence, &document); err != nil {
			return nil, nil, ErrResolutionRejected
		}
	}
	history := []cutoverAccountRequalification{}
	raw := document["account_requalifications"]
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&history); err != nil {
			return nil, nil, ErrResolutionRejected
		}
	}
	for _, record := range history {
		previous, ok := bindings[record.Role]
		if !ok || record.Qualification.PSPID != previous.Qualification.PSPID || record.Qualification.Environment != previous.Qualification.Environment || record.Qualification.Contract != previous.Qualification.Contract ||
			record.OriginalFingerprint != previous.OriginalFingerprint || record.PreviousFingerprint != previous.Fingerprint || !providerqualification.ValidFingerprint(record.CredentialFingerprint) || (record.CredentialFingerprint == previous.Fingerprint && record.CredentialVersion == previous.Version) || record.CredentialVersion < previous.Version ||
			!providerqualification.ValidEvidenceReference(record.Qualification.EvidenceRef) || record.Qualification.EvidenceRef == previous.Qualification.EvidenceRef || strings.TrimSpace(record.Actor) == "" || strings.TrimSpace(record.Reason) == "" || record.RecordedAt.IsZero() {
			return nil, nil, ErrResolutionRejected
		}
		bindings[record.Role] = cutoverAccountBinding{record.Qualification, previous.OriginalFingerprint, record.CredentialFingerprint, record.CredentialVersion, previous.Revision + 1}
	}
	return bindings, history, nil
}

func (h *NMIProviderCutover) boundCutoverClient(ctx context.Context, client *nmi.NMIClient, merchantID uuid.UUID, binding cutoverAccountBinding) bool {
	if !cutoverAccountMatches(client, merchantID, binding.Qualification.PSPID) || cutoverCredentialFingerprint(client) != binding.Fingerprint {
		return false
	}
	scopeMerchantID, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return false
	}
	row, err := h.DB.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: scopeMerchantID.UUID(), ID: binding.Qualification.PSPID})
	if err != nil || row.MerchantID != merchantID || row.Environment != binding.Qualification.Environment {
		return false
	}
	version, err := providerqualification.CredentialVersion(row)
	return err == nil && version == binding.Version
}

// writeCutover reloads the canonical operation binding while WithWrite holds
// the account share lock. Requalification takes that account's update lock, so
// a stale executor cannot dispatch after the new binding has committed.
func (h *NMIProviderCutover) writeCutover(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload, role string, client *nmi.NMIClient, send func() error) (entered bool, err error) {
	authorized, _, err := cutoverAccountBindings(in, p)
	if err != nil {
		return false, err
	}
	id := p.Request.ExpectedTargetPSPID
	if role == "source" {
		id = p.Request.ExpectedSourcePSPID
	}
	_, err = providerqualification.WithWrite(ctx, h.DB, id, cutoverCredentialFingerprint(client), func() error {
		current, err := NewStore(h.DB).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		bindings, _, err := cutoverAccountBindings(current, p)
		if err != nil {
			return err
		}
		if !bytes.Equal(current.Payload, in.Payload) || bindings[role].Revision != authorized[role].Revision || !h.boundCutoverClient(ctx, client, in.MerchantID, bindings[role]) {
			return ErrResolutionRejected
		}
		qualification, err := providerqualification.Read(ctx, h.DB, id)
		if err != nil {
			return err
		}
		if qualification.Environment != bindings[role].Qualification.Environment || qualification.Contract != bindings[role].Qualification.Contract {
			return ErrResolutionRejected
		}
		entered = true
		return send()
	})
	return entered, err
}

func (h *NMIProviderCutover) requalifyAccount(ctx context.Context, in gen.OpenrailsRailIntent, p nmiCutoverPayload, r Resolution) (Outcome, error) {
	bindings, history, err := cutoverAccountBindings(in, p)
	if err != nil {
		return Outcome{}, RejectResolution("accepted account binding is invalid")
	}
	previous, ok := bindings[r.Step]
	if !ok || !providerqualification.ValidEvidenceReference(r.RequalifyAccount) || r.RequalifyAccount == previous.Qualification.EvidenceRef {
		return Outcome{}, RejectResolution("a new account qualification reference is required")
	}
	client, ok, err := h.Resolver.ResolveNMIClient(ctx, in.MerchantID, &previous.Qualification.PSPID)
	if err != nil || !ok || client == nil || !cutoverAccountMatches(client, in.MerchantID, previous.Qualification.PSPID) {
		return Outcome{}, ErrResolutionRejected
	}
	fingerprint := cutoverCredentialFingerprint(client)
	if !providerqualification.ValidFingerprint(fingerprint) || strings.TrimSpace(r.Actor) == "" || strings.TrimSpace(r.Reason) == "" {
		return Outcome{}, ErrResolutionRejected
	}
	err = h.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := h.DB.NewWithPgxTx(tx).Gen(ctx)
		row, err := q.GetPSPForQualificationUpdate(ctx, gen.GetPSPForQualificationUpdateParams{ID: previous.Qualification.PSPID, MerchantID: in.MerchantID})
		if err != nil {
			return err
		}
		current, err := providerqualification.Current(row)
		if err != nil || current == nil || current.CredentialFingerprint != fingerprint || current.PSPID != previous.Qualification.PSPID || current.Environment != previous.Qualification.Environment || current.Contract != previous.Qualification.Contract || current.EvidenceRef != r.RequalifyAccount || *current.CredentialVersion < previous.Version || (current.CredentialFingerprint == previous.Fingerprint && *current.CredentialVersion == previous.Version) {
			return RejectResolution("current qualification does not bind this credential and account")
		}
		canonical, err := NewStore(h.DB).Get(ctx, in.ID)
		if err != nil {
			return err
		}
		_, progress, err := decodeCutover(canonical)
		if err != nil {
			return err
		}
		methodID, instrument, subscriptionID := p.SourcePaymentMethodID, p.SourceInstrument, p.SourceSubscriptionID
		if r.Step == "target" {
			methodID, instrument = p.Request.TargetPaymentMethodID, p.TargetInstrument
			subscriptionID = ""
			if progress.Target != nil {
				subscriptionID = progress.Target.ID
			}
		}
		scopeMerchantID, scopeErr := merchant.Require(ctx)
		if scopeErr != nil {
			return scopeErr
		}
		method, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: scopeMerchantID.UUID(), ID: methodID})
		if err != nil || method.MerchantID != in.MerchantID || method.CustomerID != p.CustomerID || instrument.Matches(method, charge.AgreementRecurring) != nil {
			return ErrResolutionRejected
		}
		if subscriptionID != "" {
			observed, _, err := client.GetCutoverSubscription(ctx, subscriptionID)
			if err != nil || observed.ID != subscriptionID || observed.CustomerVaultID != instrument.RailCustomerRef {
				return ErrResolutionRejected
			}
		} else if err = client.ConfirmCutoverVault(ctx, instrument.RailCustomerRef, instrument.RailMethodRef); err != nil {
			return ErrResolutionRejected
		}
		record := cutoverAccountRequalification{Role: r.Step, Qualification: current.Record, OriginalFingerprint: previous.OriginalFingerprint, PreviousFingerprint: previous.Fingerprint, CredentialFingerprint: fingerprint, CredentialVersion: *current.CredentialVersion, Actor: r.Actor, Reason: r.Reason, RecordedAt: h.now()}
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		before, err := json.Marshal(history)
		if err != nil {
			return err
		}
		n, err := q.AppendProviderCutoverAccountRequalification(ctx, gen.AppendProviderCutoverAccountRequalificationParams{ID: in.ID, MerchantID: in.MerchantID, AcceptedPayload: in.Payload, Previous: before, Record: raw})
		if err != nil {
			return err
		}
		if n != 1 {
			return RejectResolution("account binding changed while requalification was being checked")
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrResolutionRejected
		}
		return Outcome{}, err
	}
	return Retryable("operator qualified the rotated credential for this account; continue under current write gates"), nil
}

func cutoverAccountRequalificationMatches(in gen.OpenrailsRailIntent, r Resolution) bool {
	p, _, err := decodeCutover(in)
	if err != nil {
		return false
	}
	_, history, err := cutoverAccountBindings(in, p)
	if err != nil {
		return false
	}
	for i := len(history) - 1; i >= 0; i-- {
		entry := history[i]
		if entry.Role == r.Step {
			return entry.Qualification.EvidenceRef == r.RequalifyAccount && entry.Actor == r.Actor && entry.Reason == r.Reason
		}
	}
	return false
}
