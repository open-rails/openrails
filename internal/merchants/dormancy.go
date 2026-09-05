// or#914 item 5: the dormant-merchant sweep (ak#264 ruling 5 v3 — authkit has
// ZERO dormancy machinery; the HOST owns the policy; tensorhub th#1774 is the
// reference shape). A hosted product accretes never-used merchants: a user
// claims a name, connects nothing, and camps it forever. The sweep selects
// never-used merchants from openrails' own tables (no provider connected, no
// money movement, no catalog, no customers — probed per merchant under
// MerchantTx so RLS answers truthfully), warns (persisted notice + loud log),
// and after the warning lead deletes the merchant group with an EXPLICIT
// ReleaseSlug — freeing the camped name — plus a directory-row soft-delete.
// Everywhere else a delete keeps authkit's fail-safe default: tombstone
// forever. Deletion only happens on an ARMED pass; the default is a dry run.
package merchants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/pkg/merchant"
)

// GroupReleaser deletes a merchant's permission group WITH its slug released
// (authkit DeletePermissionGroupOptions{ReleaseSlug: true}). It reports nil
// for an already-deleted group so a crash between the group half and the row
// half converges on the next pass. The control plane provides one
// (pkg/embedded/controlplane.SweepDormantMerchants wires it).
type GroupReleaser func(ctx context.Context, groupID string) error

// DormancySweepConfig bounds one sweep pass. TTL and WarningLead must be
// positive; a zero Batch defaults to 200 (dormant merchants accrue by human
// signup, not by traffic).
type DormancySweepConfig struct {
	// TTL is the minimum age before a never-used merchant becomes a candidate.
	TTL time.Duration
	// WarningLead is the minimum time between the first persisted warning and
	// deletion.
	WarningLead time.Duration
	// Armed enables deletion. Default false = dry run: select, warn, log
	// would-delete lines, remove nothing.
	Armed bool
	// Batch bounds candidates per pass (default 200).
	Batch int
}

// DormancySweepResult is one pass's ledger.
type DormancySweepResult struct {
	// Candidates passed the never-used predicate this pass.
	Candidates int
	// Warned is first-time notices written this pass.
	Warned int
	// Deleted is merchants actually deleted (armed passes only).
	Deleted int
	// WouldDelete is merchants past their warning lead on an unarmed pass (or
	// with no releaser wired).
	WouldDelete int
	// Withdrawn is standing notices withdrawn because the merchant regained
	// activity or aged out of the candidate set.
	Withdrawn int
	// SkippedActive is aged merchants disqualified by the activity probe.
	SkippedActive int
}

// SweepDormant runs exactly one sweep pass. Exported so a host's periodic job
// (a River worker in openrails-saas) and the integration proof drive the SAME
// code. Fail-closed: a nil pool or non-positive TTL/lead refuses the pass; a
// nil releaser downgrades an armed pass to dry run with an error log rather
// than silently arming later.
func (s *Service) SweepDormant(ctx context.Context, cfg DormancySweepConfig, release GroupReleaser) (DormancySweepResult, error) {
	var res DormancySweepResult
	if s == nil || s.pool == nil {
		return res, errors.New("merchants: dormancy sweep requires a DB pool")
	}
	if cfg.TTL <= 0 || cfg.WarningLead <= 0 {
		return res, fmt.Errorf("merchants: non-positive dormancy ttl (%s) or warning lead (%s) — refusing to sweep", cfg.TTL, cfg.WarningLead)
	}
	batch := cfg.Batch
	if batch <= 0 {
		batch = 200
	}
	now := time.Now().UTC()
	var firstErr error
	if cfg.Armed && release != nil {
		res.Deleted, firstErr = s.resumeGroupRetirements(ctx, batch, release)
	}

	// Aged, live, GROUP-BOUND directory rows (openrails.merchants is the
	// RLS-exempt directory, so this cross-merchant read answers truthfully).
	// Merchants without a group binding are operator/embedded state, never
	// swept.
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, slug, created_at, permission_group_id
		  FROM openrails.merchants
		 WHERE deleted_at IS NULL AND status = 'active'
		   AND permission_group_id IS NOT NULL
		   AND created_at < $1
		 ORDER BY created_at
		 LIMIT $2
	`, now.Add(-cfg.TTL), batch)
	if err != nil {
		return res, fmt.Errorf("merchants: list dormancy candidates: %w", err)
	}
	type cand struct {
		id        string
		slug      string
		createdAt time.Time
		groupID   string
	}
	var aged []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.slug, &c.createdAt, &c.groupID); err != nil {
			rows.Close()
			return res, err
		}
		aged = append(aged, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	reserved := map[string]struct{}{}
	for _, r := range merchant.ReservedHostedSlugs {
		reserved[r] = struct{}{}
	}

	for _, c := range aged {
		// Defense in depth: a reserved/platform name is operator state even
		// when it looks unused; the sweep never acts on one.
		if _, ok := reserved[c.slug]; ok {
			continue
		}
		mid, err := merchant.ParseID(c.id)
		if err != nil {
			return res, err
		}

		// Probe + notice bookkeeping in ONE MerchantTx: the tenant-scoped
		// tables (including the notice ledger itself) answer under their own
		// RLS context — there is deliberately no cross-merchant notice read
		// anywhere in this sweep. A merchant that shows activity has its
		// standing notice withdrawn right here; age only grows, so every
		// notice-holder is re-probed on every pass it fits the batch.
		var (
			used          bool
			withdrew      bool
			firstWarnedAt time.Time
			warnCount     int64
		)
		err = s.pool.MerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, dormancyUsedProbeSQL, c.id).Scan(&used); err != nil {
				return err
			}
			if used {
				tag, err := tx.Exec(ctx,
					`DELETE FROM openrails.merchant_dormancy_notices WHERE merchant_id = $1::uuid`, c.id)
				withdrew = err == nil && tag.RowsAffected() > 0
				return err
			}
			return tx.QueryRow(ctx, `
				INSERT INTO openrails.merchant_dormancy_notices (merchant_id, slug)
				VALUES ($1::uuid, $2)
				ON CONFLICT (merchant_id) DO UPDATE
				   SET last_warned_at = now(),
				       warn_count = openrails.merchant_dormancy_notices.warn_count + 1,
				       slug = EXCLUDED.slug
				RETURNING first_warned_at, warn_count
			`, c.id, c.slug).Scan(&firstWarnedAt, &warnCount)
		})
		if err != nil {
			return res, fmt.Errorf("merchants: dormancy probe/notice for %s: %w", c.slug, err)
		}
		if used {
			res.SkippedActive++
			if withdrew {
				res.Withdrawn++
				log.WithFields(log.Fields{"merchant_id": c.id, "slug": c.slug}).
					Info("merchant dormancy notice withdrawn: no longer dormant (or#914)")
			}
			continue
		}
		res.Candidates++
		fields := log.Fields{
			"merchant_id": c.id, "slug": c.slug,
			"merchant_created_at": c.createdAt, "first_warned_at": firstWarnedAt,
			"warn_count": warnCount, "armed": cfg.Armed,
		}
		if warnCount == 1 {
			res.Warned++
			// The WARNING. The persisted notice plus this line IS the notice
			// (th#1774 precedent: delivery to the owner is the host's, e.g.
			// email); the warning lead is measured from it.
			log.WithFields(fields).WithField("deletable_after", firstWarnedAt.Add(cfg.WarningLead)).
				Warn("dormant merchant placed on deletion notice (or#914): never used, past ttl — will be deleted with its slug RELEASED unless it shows activity")
			continue
		}
		if now.Sub(firstWarnedAt) < cfg.WarningLead {
			continue // on notice, lead not yet served
		}
		if !cfg.Armed {
			res.WouldDelete++
			log.WithFields(fields).Warn("dormant merchant past its warning lead — DRY RUN, sweep not armed (or#914)")
			continue
		}
		if release == nil {
			res.WouldDelete++
			log.WithFields(fields).Error("dormancy sweep is ARMED but no group releaser is wired — treating as dry run (or#914)")
			continue
		}

		// Commit the tombstone and pending UUID operation BEFORE external
		// release. A restart cannot reinterpret the released name.
		retired, err := s.retireUnusedMerchant(ctx, mid, c.groupID, now, cfg.WarningLead)
		if err != nil {
			return res, err
		}
		if !retired {
			res.SkippedActive++
			continue
		}
		if err := s.releaseRetiredGroup(ctx, mid, c.groupID, release); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.WithFields(fields).WithError(err).Error("merchant retirement group release pending")
			continue
		}
		res.Deleted++

	}
	return res, firstErr
}

// dormancyUsedProbeSQL is the never-used predicate for ONE merchant, run
// inside MerchantTx. "Used" is any provider connection, money movement,
// catalog, or customer — a merchant someone invested ANY setup in is never
// dormant.
const dormancyUsedProbeSQL = `
	SELECT EXISTS (SELECT 1 FROM openrails.psps             WHERE merchant_id = $1::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.payments         WHERE merchant_id = $1::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.subscriptions    WHERE merchant_id = $1::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.customers        WHERE merchant_id = $1::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.products         WHERE merchant_id = $1::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.ledger_transfers WHERE merchant_id = $1::uuid)
`
