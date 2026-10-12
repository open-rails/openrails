package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/userinfo"

	"github.com/open-rails/openrails/billing"
)

// issuer names the shop's accounts: the Authority of its scopes and the
// Issuer of every Identity it reports.
const issuer = "https://shop.example"

// recentSignIn is how recent a sign-in must be to move money or remove
// access.
const recentSignIn = 15 * time.Minute

// rootScope is where root roles are held: every merchant. A merchant's
// scope is {issuer, the merchant's id}: the shop's own name for that
// merchant's staff. OpenRails asks Can in one or the other.
var rootScope = auth.Scope{Authority: issuer, ID: "root"}

// roles is what each role holds, by OpenRails' names. Where it is held picks
// the persona: a root role holds the root: names, a role at a merchant that
// merchant's merchant: names.
var roles = map[string][]billing.Permission{
	"admin":   billing.Permissions(),
	"support": {billing.MerchantCustomerRead, billing.MerchantCustomerManage, billing.RootCustomerRead, billing.RootCustomerManage},
	"reader":  {billing.MerchantCustomerRead, billing.RootCustomerRead},
}

// shopAuth is the shop's own auth as OpenRails takes it (authkit/auth): it
// says who a request is from shop.sessions, and what they hold from
// shop.roles, read live on every request.
type shopAuth struct{ db *pgxpool.Pool }

// Authenticate reads the session a request's bearer token names. OpenRails
// never passes cookies to an Authenticator, so a browser sends the token in
// the Authorization header.
func (a shopAuth) Authenticate(r *http.Request) (auth.Verified, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return nil, auth.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(token))
	s := &session{db: a.db}
	var merchant *string
	var revoked *time.Time
	err := a.db.QueryRow(r.Context(), `SELECT id, user_id, merchant_id, signed_in_at, revoked_at
		FROM shop.sessions WHERE token_sha256 = $1`, sum[:]).Scan(&s.id, &s.user, &merchant, &s.signedInAt, &revoked)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, auth.ErrUnauthenticated
	case err != nil:
		return nil, errors.Join(auth.ErrUnavailable, err)
	case revoked != nil:
		return nil, errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	}
	if merchant != nil {
		s.merchant = *merchant
	}
	return s, nil
}

// RootScope is where OpenRails asks for root: permissions (auth.Root).
func (shopAuth) RootScope(context.Context) (auth.Scope, error) { return rootScope, nil }

// KnownPermission lets Mount check every route's permissions
// (auth.PermissionCatalog).
func (shopAuth) KnownPermission(permission string) bool {
	return slices.Contains(billing.Permissions(), billing.Permission(permission))
}

// session is one request's verified session.
type session struct {
	db         *pgxpool.Pool
	id, user   string
	merchant   string // the merchant it acts at; "" for none
	signedInAt time.Time
}

func (s *session) Identity() auth.Identity {
	return auth.Identity{
		Issuer:      issuer,
		Subject:     s.user, // a UUID: OpenRails' customer id when the user buys
		SubjectKind: auth.SubjectUser,
		Invoker:     auth.Invoker{Issuer: issuer, ID: s.user},
		Credential:  auth.Credential{Kind: auth.CredentialSession, ID: s.id},
	}
}

// BoundScope is the merchant the session acts at (auth.Bound): OpenRails
// acts there, and nothing a request sends can change it. Zero when the
// session chose none.
func (s *session) BoundScope() auth.Scope {
	if s.merchant == "" {
		return auth.Scope{}
	}
	return auth.Scope{Authority: issuer, ID: s.merchant}
}

// Can reports whether the user holds exactly permission in exactly scope
// (auth.PermissionChecker): a root role's root: names in rootScope, a role
// at a merchant's merchant: names in that merchant's scope. A session acting
// at one merchant holds nothing at another, and keeps its root roles.
func (s *session) Can(ctx context.Context, scope auth.Scope, permission string) (bool, error) {
	p := billing.Permission(permission)
	if scope.Authority != issuer || !slices.Contains(billing.Permissions(), p) {
		return false, nil
	}
	var at *string // the roles' merchant: nil for root roles
	if scope != rootScope {
		if !canonicalUUID(scope.ID) || p.Persona() != billing.MerchantGroupPersona || (s.merchant != "" && scope.ID != s.merchant) {
			return false, nil
		}
		at = &scope.ID
	} else if p.Persona() == billing.MerchantGroupPersona {
		return false, nil
	}
	var revoked bool
	var held []string
	err := s.db.QueryRow(ctx, `SELECT s.revoked_at IS NOT NULL, coalesce(array_agg(r.role) FILTER (WHERE r.role IS NOT NULL), '{}')
		FROM shop.sessions s
		LEFT JOIN shop.roles r ON r.user_id = s.user_id AND r.merchant_id IS NOT DISTINCT FROM $2::uuid
		WHERE s.id = $1 GROUP BY s.id`, s.id, at).Scan(&revoked, &held)
	switch {
	case errors.Is(err, pgx.ErrNoRows), err == nil && revoked:
		return false, errors.Join(auth.ErrUnauthenticated, auth.ErrRevoked)
	case err != nil:
		return false, errors.Join(auth.ErrUnavailable, err)
	}
	return slices.ContainsFunc(held, func(role string) bool { return slices.Contains(roles[role], p) }), nil
}

// CheckRecentSignIn asks for a fresh sign-in once the session's is older
// than recentSignIn (auth.RecentSignInChecker).
func (s *session) CheckRecentSignIn(context.Context) error {
	if time.Since(s.signedInAt) <= recentSignIn {
		return nil
	}
	return &auth.Challenge{Err: auth.ErrStepUpRequired, MaxAge: recentSignIn}
}

// signIn starts a session for user acting at merchant ("" for none) and
// returns its bearer token. The shop's sign-in calls it once the password
// or passkey checks out.
func (a shopAuth) signIn(ctx context.Context, user, merchant string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	_, err := a.db.Exec(ctx, `INSERT INTO shop.sessions (token_sha256, user_id, merchant_id)
		VALUES ($1, $2, $3)`, sum[:], user, nullable(merchant))
	return token, err
}

// switchMerchant is POST /switch {"merchant_id": …}: staff point their
// session at a merchant where they hold a role, or any with a root role.
// Every later request of the session acts there.
func (a shopAuth) switchMerchant(merchants []billing.MerchantID) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := a.Authenticate(r)
		if err != nil {
			refuse(w, r, err)
			return
		}
		var body struct {
			MerchantID billing.MerchantID `json:"merchant_id"`
		}
		if err := decode(r, &body); err != nil || !slices.Contains(merchants, body.MerchantID) {
			http.Error(w, "unknown merchant", http.StatusBadRequest)
			return
		}
		s := v.(*session)
		tag, err := a.db.Exec(r.Context(), `UPDATE shop.sessions SET merchant_id = $2
			WHERE id = $1 AND EXISTS (SELECT 1 FROM shop.roles
				WHERE user_id = shop.sessions.user_id AND (merchant_id IS NULL OR merchant_id = $2))`, s.id, body.MerchantID.String())
		switch {
		case err != nil:
			refuse(w, r, errors.Join(auth.ErrUnavailable, err))
		case tag.RowsAffected() == 0:
			refuse(w, r, auth.ErrForbidden)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// signOut is POST /sign-out: it revokes the request's session, which every
// later request, OpenRails' included, finds revoked.
func (a shopAuth) signOut(w http.ResponseWriter, r *http.Request) {
	v, err := a.Authenticate(r)
	if err != nil {
		refuse(w, r, err)
		return
	}
	if _, err := a.db.Exec(r.Context(), `UPDATE shop.sessions SET revoked_at = now() WHERE id = $1`, v.(*session).id); err != nil {
		refuse(w, r, errors.Join(auth.ErrUnavailable, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refuse answers err as every authkit/auth consumer does.
func refuse(w http.ResponseWriter, r *http.Request, err error) {
	refusal := auth.Refuse(r, err)
	for name, values := range refusal.Header {
		w.Header()[name] = values
	}
	w.WriteHeader(refusal.Status)
}

// directory is the shop's users as OpenRails reads customers' contacts
// (Deps.UserInfo).
type directory struct{ db *pgxpool.Pool }

func (d directory) Get(ctx context.Context, ids []string) (map[string]userinfo.User, error) {
	valid := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return !canonicalUUID(id) })
	rows, err := d.db.Query(ctx, `SELECT id, email FROM shop.users WHERE id = ANY($1::uuid[])`, valid)
	if err != nil {
		return nil, err
	}
	users, err := pgx.CollectRows(rows, scanUser)
	if err != nil {
		return nil, err
	}
	out := make(map[string]userinfo.User, len(users))
	for _, u := range users {
		out[u.ID] = u
	}
	return out, nil
}

func (d directory) Search(ctx context.Context, query string, limit int) ([]userinfo.User, error) {
	if query == "" || limit < 1 {
		return nil, nil
	}
	literal := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	rows, err := d.db.Query(ctx, `SELECT id, email FROM shop.users
		WHERE email ILIKE '%' || $1 || '%' ORDER BY email LIMIT $2`, literal, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanUser)
}

func scanUser(row pgx.CollectableRow) (userinfo.User, error) {
	var u userinfo.User
	return u, row.Scan(&u.ID, &u.Email)
}

// canonicalUUID is a UUID in its one lowercase spelling, as the shop's ids
// are written: another spelling names no scope or user.
func canonicalUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// decode reads a small JSON body, refusing unknown fields.
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<12))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
