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

// #539: customer identity is merchant + stable host/AuthKit UUID subject.
// Issuer is audit metadata only; adding/removing/changing issuer URLs must not
// split one real customer.
func TestInvokerIdentityAndPayerNaturalKey(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	dbtest.EnsureTestMerchant(ctx, t, pool)
	merchantID := dbtest.TestMerchantID.UUID()
	q := gen.New(pool)

	t.Run("subject_natural_key_is_idempotent_across_issuers", func(t *testing.T) {
		issuerA := "https://doujins.example"
		issuerB := "https://hentai0.example"
		subjAID := uuid.New()
		subjBID := uuid.New()
		subjA := subjAID.String()
		subjB := subjBID.String()

		a1, err := q.UpsertCustomerBySubject(ctx, gen.UpsertCustomerBySubjectParams{MerchantID: merchantID, Issuer: &issuerA, Subject: &subjAID})
		require.NoError(t, err)
		a2, err := q.UpsertCustomerBySubject(ctx, gen.UpsertCustomerBySubjectParams{MerchantID: merchantID, Issuer: &issuerB, Subject: &subjAID})
		require.NoError(t, err)
		require.Equal(t, a1, a2, "same (merchant,subject) must survive issuer changes")

		b1, err := q.UpsertCustomerBySubject(ctx, gen.UpsertCustomerBySubjectParams{MerchantID: merchantID, Issuer: &issuerA, Subject: &subjBID})
		require.NoError(t, err)
		require.NotEqual(t, a1, b1, "distinct subjects -> distinct customers")

		// External-subjects lookup resolves both, keyed by subject.
		rows, err := q.LookupCustomerIDsBySubjects(ctx, gen.LookupCustomerIDsBySubjectsParams{MerchantID: merchantID, Subjects: []string{subjA, subjB}})
		require.NoError(t, err)
		got := map[string]uuid.UUID{}
		for _, r := range rows {
			require.NotNil(t, r.Subject)
			got[*r.Subject] = r.ID
		}
		require.Equal(t, a1, got[subjA])
		require.Equal(t, b1, got[subjB])
	})
}
