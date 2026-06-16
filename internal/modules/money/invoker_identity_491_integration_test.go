//go:build integration

package money_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #491 slice 5: prove the payer natural-key resolution (org-bound vs org-less),
// the external-subject lookup, and the cross-schema invoker_id FK end-to-end
// against the FULLY MIGRATED schema (authkit profiles first, then openrails 027),
// which also exercises the cross-schema FK ordering.
func TestInvokerIdentityAndPayerNaturalKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedPGXPool(t)
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	q := gen.New(pool)

	t.Run("org_less_natural_key_is_idempotent_and_per_subject", func(t *testing.T) {
		issuer := "doujins"
		subjA := "491-" + uuid.New().String()
		subjB := "491-" + uuid.New().String()

		a1, err := q.UpsertCustomerByIssuerSubject(ctx, gen.UpsertCustomerByIssuerSubjectParams{MerchantID: merchantID, Issuer: &issuer, Subject: &subjA})
		require.NoError(t, err)
		a2, err := q.UpsertCustomerByIssuerSubject(ctx, gen.UpsertCustomerByIssuerSubjectParams{MerchantID: merchantID, Issuer: &issuer, Subject: &subjA})
		require.NoError(t, err)
		require.Equal(t, a1, a2, "same (merchant,issuer,subject) -> same uuidv7 id")

		b1, err := q.UpsertCustomerByIssuerSubject(ctx, gen.UpsertCustomerByIssuerSubjectParams{MerchantID: merchantID, Issuer: &issuer, Subject: &subjB})
		require.NoError(t, err)
		require.NotEqual(t, a1, b1, "distinct subjects -> distinct org-less payers")

		// External-subjects lookup resolves both, keyed by subject.
		rows, err := q.LookupCustomerIDsByIssuerSubjects(ctx, gen.LookupCustomerIDsByIssuerSubjectsParams{MerchantID: merchantID, Issuer: &issuer, Subjects: []string{subjA, subjB}})
		require.NoError(t, err)
		got := map[string]uuid.UUID{}
		for _, r := range rows {
			require.NotNil(t, r.Subject)
			got[*r.Subject] = r.ID
		}
		require.Equal(t, a1, got[subjA])
		require.Equal(t, b1, got[subjB])
	})

	t.Run("org_bound_collapses_many_subjects_to_one_payer", func(t *testing.T) {
		org := "org-" + uuid.New().String()
		c1, err := q.UpsertCustomerByOrg(ctx, gen.UpsertCustomerByOrgParams{MerchantID: merchantID, OrgID: &org})
		require.NoError(t, err)
		c2, err := q.UpsertCustomerByOrg(ctx, gen.UpsertCustomerByOrgParams{MerchantID: merchantID, OrgID: &org})
		require.NoError(t, err)
		require.Equal(t, c1, c2, "org-bound issuer -> ONE payer per org regardless of subject")
	})
}
