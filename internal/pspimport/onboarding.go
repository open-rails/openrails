package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

// CredentialsFindingType is the finding a pull the provider refused the
// credentials of opens, one per PSP (subject: its id), until a pull of the PSP
// reads it again or the PSP is no longer importable.
const CredentialsFindingType = "consistency.psp_import.credentials_rejected"

// ErrCredentialsRejected is the provider refusing the PSP's credentials: an
// NMI v5 401/403 or Query API authentication error, a Stripe 401/403, a
// DataLink 401/403 or authentication failure.
var ErrCredentialsRejected = errors.New("the provider rejected the PSP's credentials")

// rejected reports whether err is a provider refusing the credentials.
func rejected(err error) bool {
	var stripe *reconcile.StripeAPIError
	return errors.Is(err, ErrCredentialsRejected) || errors.Is(err, nmi.ErrCredentialsRejected) || errors.Is(err, ccbill.ErrDataLinkAuth) ||
		errors.As(err, &stripe) && (stripe.Status == 401 || stripe.Status == 403)
}

// importable is a PSP an onboarding import pulls: a declared, live account on
// a rail with an import.
func importable(scope merchants.PSPScope) bool {
	_, ok := sources[scope.Rail]
	return ok && scope.Declared && !scope.Archived && !scope.Superseded && !scope.DuplicateAccount
}

// EnsureOnboarding gives each importable PSP of psps its onboarding import:
// created with its pull job when the PSP has none, in any status; restarted in
// place when it failed and the PSP is in rearm. A concurrent replica or host
// creating or restarting the same one does nothing. A manual import never
// stands in for it. The credentials finding of a PSP no longer importable is
// resolved. A PSP another import is pulling or applying is passed over; the
// others proceed and the error then names it (billing.ErrPSPImportInProgress),
// for the caller to retry once that import finishes.
func (im *Importer) EnsureOnboarding(ctx context.Context, mid billing.MerchantID, psps []merchants.PSPScope, rearm map[uuid.UUID]bool) error {
	ctx = merchant.WithID(ctx, mid)
	rules, err := encodeRules(nil)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	var busy []string
	for _, scope := range psps {
		if !importable(scope) {
			continue
		}
		live[billing.PSPID(scope.ID).String()] = true
		err := im.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			d := im.DB.NewWithPgxTx(tx)
			q := d.Gen(ctx)
			rows, err := q.CreateOnboardingPSPImport(ctx, gen.CreateOnboardingPSPImportParams{MerchantID: mid.UUID(), PspID: scope.ID, Rail: scope.Rail, Rules: rules})
			if err != nil {
				return err
			}
			if len(rows) == 0 && rearm[scope.ID] {
				if rows, err = q.RearmOnboardingPSPImport(ctx, gen.RearmOnboardingPSPImportParams{MerchantID: mid.UUID(), PspID: scope.ID}); err != nil {
					return err
				}
			}
			if len(rows) == 0 {
				return nil
			}
			_, err = d.InsertRiverJob(ctx, JobArgs{MerchantID: mid.UUID(), ImportID: rows[0].ID, Step: StepPull}, nil)
			return err
		})
		if errors.Is(inFlight(err), errInFlight) {
			busy = append(busy, scope.Key)
			continue
		}
		if err != nil {
			return fmt.Errorf("onboarding import of PSP %s: %w", scope.Key, err)
		}
	}
	err = im.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := im.DB.Gen(ctx)
		findingType, open := CredentialsFindingType, "requires_review"
		listed, err := q.ListReconciliationFindings(ctx, gen.ListReconciliationFindingsParams{MerchantID: mid.UUID(), FindingType: &findingType, Status: &open, PageLimit: 1000})
		if err != nil {
			return err
		}
		for _, f := range listed {
			if live[f.SubjectKey] {
				continue
			}
			if _, err := q.ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: mid.UUID(), FindingType: CredentialsFindingType, SubjectKey: f.SubjectKey}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || len(busy) == 0 {
		return err
	}
	return apperr.New(409, billing.CodePSPImportInProgress, fmt.Sprintf("another import of PSP %s is pulling or applying; its onboarding import starts once it finishes", strings.Join(busy, ", ")))
}

// openCredentialsFinding records that the provider refused the import's PSP's
// credentials.
func openCredentialsFinding(ctx context.Context, q *gen.Queries, row gen.BillingPspImport, pspKey, cause string) (gen.BillingReconciliationFinding, error) {
	psp := billing.PSPID(row.PspID).String()
	action := fmt.Sprintf("The provider refused PSP %s's credentials when import %s pulled it. Correct them; the onboarding import restarts once the PSP's configuration changes or is next loaded.",
		pspKey, billing.PSPImportID(row.ID))
	evidence, err := json.Marshal(map[string]string{"psp": psp, "psp_key": pspKey, "rail": row.Rail, "import": billing.PSPImportID(row.ID).String(), "error": cause})
	if err != nil {
		return gen.BillingReconciliationFinding{}, err
	}
	return q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: row.MerchantID, FindingType: CredentialsFindingType,
		SubjectKey: psp, Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence})
}

// resolveCredentialsFinding closes the PSP's credentials finding: a pull read
// it.
func (im *Importer) resolveCredentialsFinding(ctx context.Context, row gen.BillingPspImport) error {
	_, err := im.DB.Gen(ctx).ResolveConfigFinding(ctx, gen.ResolveConfigFindingParams{MerchantID: row.MerchantID, FindingType: CredentialsFindingType,
		SubjectKey: billing.PSPID(row.PspID).String()})
	return err
}
