//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Alter a row and recompute the footer: these tests exercise semantic rejection,
// rather than merely discovering that modifying bytes invalidates the digest.
func alteredArchive(t *testing.T, raw []byte, table, column string, value *string) []byte {
	t.Helper()
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	current, changed := "", false
	for i := 1; i < len(lines)-1; i++ {
		var r archivewire.Record
		require.NoError(t, json.Unmarshal(lines[i], &r))
		if r.Kind == "table" {
			current = r.Table
		}
		if r.Kind != "row" || current != table || changed {
			continue
		}
		for _, p := range contract.Profiles {
			if p.Name != table {
				continue
			}
			for j, c := range p.Columns {
				if c.Name == column {
					r.Values[j] = value
					changed = true
				}
			}
		}
		var err error
		lines[i], err = json.Marshal(r)
		require.NoError(t, err)
	}
	require.True(t, changed, "archive fixture must contain the requested row")
	var footer archivewire.Record
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &footer))
	body := append(bytes.Join(lines[:len(lines)-1], []byte{'\n'}), '\n')
	sum := sha256.Sum256(body)
	footer.Digest = hex.EncodeToString(sum[:])
	encoded, err := json.Marshal(footer)
	require.NoError(t, err)
	return append(append(body, encoded...), '\n')
}

func assertEmptyBook(t *testing.T, d *db.DB, id merchant.ID) {
	t.Helper()
	require.NoError(t, d.MerchantTx(merchant.WithID(t.Context(), id), func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range contract.Profiles {
			var n int
			require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails."+p.Name+" WHERE merchant_id=$1", id.UUID()).Scan(&n))
			require.Zero(t, n, p.Name)
		}
		return nil
	}))
}

func TestRestoreRejectsTamperingAtomically(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "archive_adversarial")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	seedBook(t, source, id)
	var original bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &original))
	text := func(v string) *string { return &v }
	for _, tc := range []struct {
		name, table, column string
		value               *string
	}{
		{"blank primary key", "customers", "id", text("")},
		{"null primary key", "customers", "id", nil},
		{"foreign merchant", "customers", "merchant_id", text(uuid.NewString())},
		{"unbalanced ledger counters", "ledger_accounts", "credits_posted", text("7")},
		{"unsafe opaque intent evidence", "rail_intents", "result_evidence", text(`{"transaction_id":"retained","raw_body":{"secret":"must-refuse"}}`)},
		{"unsafe checkout replay state", "checkout_sessions", "rail_state", text(`{"_openrails_request_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","unknown":"must-refuse"}`)},
		{"unacknowledged event", "host_outbox", "delivered_at", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, original.Bytes(), tc.table, tc.column, tc.value)))
			require.Error(t, err)
			assertEmptyBook(t, target, id)
		})
	}
	broken := append([]byte(nil), original.Bytes()...)
	broken[len(broken)-5] ^= 1
	_, err := Restore(t.Context(), target, id, bytes.NewReader(broken))
	require.Error(t, err)
	assertEmptyBook(t, target, id)
	_, err = Restore(t.Context(), target, id, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	// A committed import whose HTTP response was lost is replayed after normal
	// activity, without requiring the now-live destination to still be empty.
	ctx := merchant.WithID(t.Context(), id)
	require.NoError(t, target.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)", id.UUID(), uuid.New())
		return err
	}))
	replay, err := Restore(t.Context(), target, id, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	_, err = Restore(t.Context(), target, id, bytes.NewReader(alteredArchive(t, original.Bytes(), "customers", "issuer", text("https://different.example"))))
	require.Error(t, err)
}

func TestRestoreGuardRejectsForgeryForeignScopeAndNullFinish(t *testing.T) {
	d := archiveDB(t, "archive_guard")
	id, other := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	provision(t, d, id)
	provision(t, d, other)
	ctx := merchant.WithID(t.Context(), id)
	err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A temporary catalog namesake must not change the owner check or the
		// SECURITY DEFINER destination-table enumeration.
		_, err := tx.Exec(ctx, "CREATE TEMP TABLE pg_class AS SELECT oid,(SELECT oid FROM pg_roles WHERE rolname=current_user) AS relowner FROM pg_catalog.pg_class")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO openrails.maintenance_runs(merchant_id,kind,actor) VALUES($1,'billing_restore','merchantarchive')", id.UUID())
		return err
	})
	require.ErrorContains(t, err, "restore receipts")
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT openrails.begin_billing_restore($1)", other.UUID())
		return err
	})
	require.ErrorContains(t, err, "merchant mismatch")
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT openrails.begin_billing_restore($1)", id.UUID()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT openrails.finish_billing_restore($1,NULL,NULL)", id.UUID())
		return err
	})
	require.ErrorContains(t, err, "invalid billing restore finalization")
	assertEmptyBook(t, d, id)
	var empty bytes.Buffer
	require.NoError(t, Export(t.Context(), d, id, &empty))
	_, err = Restore(t.Context(), d, id, bytes.NewReader(empty.Bytes()))
	require.NoError(t, err)
	for _, query := range []string{
		"UPDATE openrails.maintenance_runs SET summary='{}' WHERE merchant_id=$1 AND kind='billing_restore'",
		"DELETE FROM openrails.maintenance_runs WHERE merchant_id=$1 AND kind='billing_restore'",
	} {
		err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, query, id.UUID()); return err })
		require.Error(t, err)
	}
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var receipt string
		require.NoError(t, tx.QueryRow(ctx, "SELECT id::text FROM openrails.maintenance_runs WHERE merchant_id=$1 AND kind='billing_restore'", id.UUID()).Scan(&receipt))
		_, err := tx.Exec(ctx, "SELECT set_config('app.billing_restore_id',$1,true)", receipt)
		require.NoError(t, err)
		var active bool
		require.NoError(t, tx.QueryRow(ctx, "SELECT openrails.billing_restore_active($1)", id.UUID()).Scan(&active))
		require.False(t, active)
		return nil
	}))
}

// Stop after the header has been processed and the destination lock acquired.
type pausedArchive struct {
	header, rest *bytes.Reader
	waiting      chan struct{}
	release      chan struct{}
	waited       bool
}

func (r *pausedArchive) Read(p []byte) (int, error) {
	if r.header.Len() > 0 {
		return r.header.Read(p)
	}
	if !r.waited {
		r.waited = true
		close(r.waiting)
		<-r.release
	}
	return r.rest.Read(p)
}
func TestRestoreSerializesConcurrentReceipts(t *testing.T) {
	d := archiveDB(t, "archive_concurrent")
	id := merchant.ID(uuid.New())
	provision(t, d, id)
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), d, id, &artifact))
	raw := artifact.Bytes()
	n := bytes.IndexByte(raw, '\n') + 1
	paused := &pausedArchive{header: bytes.NewReader(raw[:n]), rest: bytes.NewReader(raw[n:]), waiting: make(chan struct{}), release: make(chan struct{})}
	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	go func() { r, e := Restore(t.Context(), d, id, paused); outcomes <- outcome{r, e} }()
	<-paused.waiting
	started := make(chan struct{})
	go func() {
		close(started)
		r, e := Restore(t.Context(), d, id, bytes.NewReader(raw))
		outcomes <- outcome{r, e}
	}()
	<-started
	close(paused.release)
	first, second := <-outcomes, <-outcomes
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotEqual(t, first.result.Replayed, second.result.Replayed)
	require.Equal(t, first.result.Digest, second.result.Digest)
}

func TestArchiveRejectsOwnedColumnDriftAndIgnoresHostTables(t *testing.T) {
	d := archiveDB(t, "archive_schema_policy")
	id := merchant.ID(uuid.New())
	provision(t, d, id)
	admin, err := pgx.Connect(t.Context(), dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	defer admin.Close(t.Context())
	for _, tc := range []struct{ name, add, remove string }{
		{"retained column", "ALTER TABLE archive_schema_policy.customers ADD COLUMN future_fact text", "ALTER TABLE archive_schema_policy.customers DROP COLUMN future_fact"},
		{"excluded table column", "ALTER TABLE archive_schema_policy.notifications ADD COLUMN future_fact text", "ALTER TABLE archive_schema_policy.notifications DROP COLUMN future_fact"},
		{"merchant table", "CREATE TABLE archive_schema_policy.future_book(merchant_id uuid)", "DROP TABLE archive_schema_policy.future_book"},
		{"unscoped table", "CREATE TABLE archive_schema_policy.future_global_fact(id uuid)", "DROP TABLE archive_schema_policy.future_global_fact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.Exec(t.Context(), tc.add)
			require.NoError(t, err)
			err = Export(t.Context(), d, id, io.Discard)
			if strings.HasPrefix(tc.add, "CREATE TABLE") {
				require.NoError(t, err, "an undeclared co-located host table is not OpenRails ownership")
			} else {
				var ae *Error
				require.ErrorAs(t, err, &ae)
				require.Equal(t, "unsupported_state", ae.Code)
			}
			_, err = admin.Exec(t.Context(), tc.remove)
			require.NoError(t, err)
		})
	}
	require.NoError(t, Export(t.Context(), d, id, io.Discard))
}

func TestArchiveOpaqueMetadataRefusesBeforeHeader(t *testing.T) {
	d := archiveDB(t, "openrails")
	id := merchant.ID(uuid.New())
	provision(t, d, id)
	seedBook(t, d, id)
	admin, err := pgx.Connect(t.Context(), dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	defer admin.Close(t.Context())
	ctx := t.Context()
	for _, tc := range []struct{ table, column string }{{"payments", "discount_metadata"}, {"payments", "metadata"}, {"payment_methods", "metadata"}, {"usage_events", "metadata"}, {"invoice_items", "metadata"}, {"maintenance_runs", "affected"}} {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			_, err := admin.Exec(ctx, "UPDATE openrails."+tc.table+" SET "+tc.column+"='{}'::jsonb || jsonb_build_object('once_only','retained') WHERE merchant_id=$1", id.UUID())
			require.NoError(t, err)
			var out bytes.Buffer
			err = Export(t.Context(), d, id, &out)
			require.Error(t, err)
			require.Zero(t, out.Len())
			_, err = admin.Exec(ctx, "UPDATE openrails."+tc.table+" SET "+tc.column+"='{}'::jsonb WHERE merchant_id=$1", id.UUID())
			if tc.table == "maintenance_runs" {
				_, err = admin.Exec(ctx, "UPDATE openrails.maintenance_runs SET affected=NULL WHERE merchant_id=$1", id.UUID())
			}
			require.NoError(t, err)
		})
	}
	require.NoError(t, Export(t.Context(), d, id, io.Discard))
}

func TestRestoreRequiresEmptyProvisionedBookAndPreservesAuthority(t *testing.T) {
	source, target := archiveDB(t, "openrails"), archiveDB(t, "archive_authority")
	id, foreign := merchant.ID(uuid.New()), merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	provision(t, target, foreign)
	seedBook(t, source, id)
	var artifact bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &artifact))
	_, err := Restore(t.Context(), target, foreign, bytes.NewReader(artifact.Bytes()))
	var ae *Error
	require.ErrorAs(t, err, &ae)
	require.Equal(t, "merchant_mismatch", ae.Code)
	ctx := merchant.WithID(t.Context(), id)
	require.NoError(t, target.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)", id.UUID(), uuid.New())
		return err
	}))
	_, err = Restore(t.Context(), target, id, bytes.NewReader(artifact.Bytes()))
	require.ErrorAs(t, err, &ae)
	require.Equal(t, "not_empty", ae.Code)
	// Another explicitly provisioned destination gets its own new host/group,
	// while customer and provider domain IDs from the archive stay unchanged.
	clean := archiveDB(t, "archive_authority_clean")
	provision(t, clean, id)
	_, err = clean.Qx(t.Context()).Exec(t.Context(), "UPDATE openrails.merchants SET permission_group_id='new-authority-group',api_host=$2 WHERE id=$1", id.UUID(), "archive-"+id.String()+".example.test")
	require.NoError(t, err)
	_, err = Restore(t.Context(), clean, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	var group, host string
	require.NoError(t, clean.Qx(t.Context()).QueryRow(t.Context(), "SELECT permission_group_id,api_host FROM openrails.merchants WHERE id=$1", id.UUID()).Scan(&group, &host))
	require.Equal(t, "new-authority-group", group)
	require.Equal(t, "archive-"+id.String()+".example.test", host)
}
