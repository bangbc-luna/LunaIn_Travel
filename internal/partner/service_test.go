package partner_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/bangbc-luna/lunain-travel/internal/identity"
	"github.com/bangbc-luna/lunain-travel/internal/partner"
	"github.com/bangbc-luna/lunain-travel/internal/platform/auth"
	"github.com/bangbc-luna/lunain-travel/internal/platform/db"
)

func (e *env) enabled(t *testing.T, partnerID uuid.UUID, code string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		ok, err = e.Svc.IsServiceTypeEnabled(context.Background(), tx, partnerID, code)
		return err
	}))
	return ok
}

func (e *env) status(t *testing.T, partnerID uuid.UUID, lock bool) string {
	t.Helper()
	var st string
	require.NoError(t, db.InTx(context.Background(), e.Pool, func(tx pgx.Tx) error {
		var err error
		st, err = e.Svc.Status(context.Background(), tx, partnerID, lock)
		return err
	}))
	return st
}

func TestCreate_CreatesUserPartnerMemberAndServiceTypes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Out of sort_order and with a duplicate: stored once, returned by sort_order.
	in := input("alpha", "private_charter", "attraction_ticket", "private_charter")
	in.OwnerEmail = "  Owner@Alpha.VN "
	p := e.create(t, in)

	require.NotEqual(t, uuid.Nil, p.ID)
	require.Equal(t, "Công ty alpha", p.LegalName)
	require.Equal(t, "alpha", p.DisplayName)
	require.Equal(t, "active", p.Status)
	require.Nil(t, p.SuspendedAt)
	require.Equal(t, []string{"attraction_ticket", "private_charter"}, p.ServiceTypes)
	require.True(t, p.CreatedAt.Equal(testNow))

	tok, err := e.Identity.Login(ctx, "owner@alpha.vn", ownerPassword)
	require.NoError(t, err)
	require.Equal(t, auth.RolePartner, tok.Role)

	var memberUser uuid.UUID
	var role string
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT user_id, role FROM partner_members WHERE partner_id = $1`, p.ID).Scan(&memberUser, &role))
	require.Equal(t, p.OwnerUserID, memberUser)
	require.Equal(t, "owner", role)
	require.Equal(t, 2, e.count(t, "partner_service_types"))

	got, err := e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, p, got)

	require.Equal(t, 1, e.auditCount(t, "partner.create"))
	var actor, target uuid.UUID
	var after string
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT actor_user_id, target_id, after::text FROM admin_audit_log WHERE action = 'partner.create'`).
		Scan(&actor, &target, &after))
	require.Equal(t, e.Admin, actor)
	require.Equal(t, p.ID, target)
	require.NotContains(t, after, ownerPassword)
	require.Contains(t, after, "owner@alpha.vn")
}

func TestCreate_EmptyServiceTypes(t *testing.T) {
	e := newEnv(t)
	p := e.create(t, input("beta"))
	require.Empty(t, p.ServiceTypes)
	require.NotNil(t, p.ServiceTypes)
}

func TestCreate_ValidatesNames(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before := e.counts(t)
	for name, mutate := range map[string]func(*partner.CreateInput){
		"empty legal":      func(in *partner.CreateInput) { in.LegalName = "  " },
		"empty display":    func(in *partner.CreateInput) { in.DisplayName = "" },
		"long legal":       func(in *partner.CreateInput) { in.LegalName = strings.Repeat("ă", 201) },
		"long display":     func(in *partner.CreateInput) { in.DisplayName = strings.Repeat("a", 201) },
		"long contact":     func(in *partner.CreateInput) { in.ContactName = strings.Repeat("a", 201) },
		"bad contact mail": func(in *partner.CreateInput) { in.ContactEmail = "not-an-email" },
	} {
		in := input("gamma")
		mutate(&in)
		_, err := e.Svc.Create(ctx, e.Admin, in)
		status, code := appCode(t, err)
		require.Equal(t, http.StatusUnprocessableEntity, status, name)
		require.Equal(t, "invalid_partner", code, name)
	}
	require.Equal(t, before, e.counts(t))

	in := input("gamma")
	in.LegalName = strings.Repeat("ă", 200)
	in.ContactEmail = ""
	e.create(t, in)
}

func TestCreate_DuplicateOwnerEmailLeavesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.create(t, input("alpha", "attraction_ticket"))
	before := e.counts(t)

	in := input("beta", "attraction_ticket")
	in.OwnerEmail = "OWNER@alpha.vn"
	_, err := e.Svc.Create(ctx, e.Admin, in)
	require.ErrorIs(t, err, identity.ErrEmailTaken)
	require.Equal(t, before, e.counts(t))

	// The owner email of an existing super_admin is also taken.
	in.OwnerEmail = "admin@lunain.vn"
	_, err = e.Svc.Create(ctx, e.Admin, in)
	require.ErrorIs(t, err, identity.ErrEmailTaken)
	require.Equal(t, before, e.counts(t))
}

func TestCreate_UnknownServiceTypeLeavesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	before := e.counts(t)

	_, err := e.Svc.Create(ctx, e.Admin, input("alpha", "attraction_ticket", "nope"))
	status, code := appCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "invalid_service_type", code)
	require.Equal(t, before, e.counts(t))

	_, err = e.Identity.Login(ctx, "owner@alpha.vn", ownerPassword)
	require.ErrorIs(t, err, identity.ErrInvalidCredentials)
}

func TestGet_NotFound(t *testing.T) {
	e := newEnv(t)
	_, err := e.Svc.Get(context.Background(), uuid.Must(uuid.NewV7()))
	require.ErrorIs(t, err, partner.ErrPartnerNotFound)
	status, _ := appCode(t, err)
	require.Equal(t, http.StatusNotFound, status)
}

func TestEnableDisableServiceType(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.create(t, input("alpha", "attraction_ticket"))
	require.True(t, e.enabled(t, p.ID, "attraction_ticket"))
	require.False(t, e.enabled(t, p.ID, "scheduled_ride"))

	require.NoError(t, e.Svc.DisableServiceType(ctx, e.Admin, p.ID, "attraction_ticket"))
	require.False(t, e.enabled(t, p.ID, "attraction_ticket"))
	got, err := e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Empty(t, got.ServiceTypes)
	// Disabling again is a no-op.
	require.NoError(t, e.Svc.DisableServiceType(ctx, e.Admin, p.ID, "attraction_ticket"))

	require.NoError(t, e.Svc.EnableServiceType(ctx, e.Admin, p.ID, "attraction_ticket"))
	require.True(t, e.enabled(t, p.ID, "attraction_ticket"))
	var disabledAt, disabledBy any
	var enabledBy uuid.UUID
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT disabled_at, disabled_by, enabled_by FROM partner_service_types WHERE partner_id = $1 AND service_type = 'attraction_ticket'`,
		p.ID).Scan(&disabledAt, &disabledBy, &enabledBy))
	require.Nil(t, disabledAt)
	require.Nil(t, disabledBy)
	require.Equal(t, e.Admin, enabledBy)

	// Enabling twice is fine.
	require.NoError(t, e.Svc.EnableServiceType(ctx, e.Admin, p.ID, "attraction_ticket"))
	require.NoError(t, e.Svc.EnableServiceType(ctx, e.Admin, p.ID, "scheduled_ride"))
	require.NoError(t, e.Svc.EnableServiceType(ctx, e.Admin, p.ID, "scheduled_ride"))
	require.True(t, e.enabled(t, p.ID, "scheduled_ride"))
	got, err = e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"attraction_ticket", "scheduled_ride"}, got.ServiceTypes)

	// Audit: one row per actual change (disable, re-enable, enable new).
	require.Equal(t, 1, e.auditCount(t, "partner.service_type.disable"))
	require.Equal(t, 2, e.auditCount(t, "partner.service_type.enable"))

	// Never enabled → 404; unknown code → 422; unknown partner → 404.
	err = e.Svc.DisableServiceType(ctx, e.Admin, p.ID, "private_charter")
	require.ErrorIs(t, err, partner.ErrServiceTypeNotEnabled)
	status, _ := appCode(t, err)
	require.Equal(t, http.StatusNotFound, status)

	_, code := appCode(t, e.Svc.EnableServiceType(ctx, e.Admin, p.ID, "nope"))
	require.Equal(t, "invalid_service_type", code)
	require.ErrorIs(t, e.Svc.EnableServiceType(ctx, e.Admin, uuid.Must(uuid.NewV7()), "attraction_ticket"), partner.ErrPartnerNotFound)
	require.ErrorIs(t, e.Svc.DisableServiceType(ctx, e.Admin, uuid.Must(uuid.NewV7()), "attraction_ticket"), partner.ErrPartnerNotFound)
}

func TestSuspendUnsuspend(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	p := e.create(t, input("alpha", "attraction_ticket"))

	_, code := appCode(t, e.Svc.Suspend(ctx, e.Admin, p.ID, "   "))
	require.Equal(t, "invalid_suspend_reason", code)

	require.NoError(t, e.Svc.Suspend(ctx, e.Admin, p.ID, " Vi phạm điều khoản "))
	got, err := e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "suspended", got.Status)
	require.NotNil(t, got.SuspendedAt)
	require.True(t, got.SuspendedAt.Equal(testNow))
	require.Equal(t, "Vi phạm điều khoản", *got.SuspendedReason)
	require.Equal(t, "suspended", e.status(t, p.ID, true))
	require.Equal(t, "suspended", e.status(t, p.ID, false))

	err = e.Svc.Suspend(ctx, e.Admin, p.ID, "lần nữa")
	require.ErrorIs(t, err, partner.ErrAlreadySuspended)
	status, code := appCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "partner_already_suspended", code)

	// The partner's user can still log in while suspended (spec §5.2).
	_, err = e.Identity.Login(ctx, "owner@alpha.vn", ownerPassword)
	require.NoError(t, err)

	require.NoError(t, e.Svc.Unsuspend(ctx, e.Admin, p.ID))
	got, err = e.Svc.Get(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, "active", got.Status)
	require.Nil(t, got.SuspendedAt)
	require.Nil(t, got.SuspendedReason)

	err = e.Svc.Unsuspend(ctx, e.Admin, p.ID)
	require.ErrorIs(t, err, partner.ErrNotSuspended)
	status, _ = appCode(t, err)
	require.Equal(t, http.StatusConflict, status)

	require.Equal(t, 1, e.auditCount(t, "partner.suspend"))
	require.Equal(t, 1, e.auditCount(t, "partner.unsuspend"))
	var reason string
	require.NoError(t, e.Pool.QueryRow(ctx,
		`SELECT reason FROM admin_audit_log WHERE action = 'partner.suspend'`).Scan(&reason))
	require.Equal(t, "Vi phạm điều khoản", reason)

	require.ErrorIs(t, e.Svc.Suspend(ctx, e.Admin, uuid.Must(uuid.NewV7()), "x"), partner.ErrPartnerNotFound)
	err = db.InTx(ctx, e.Pool, func(tx pgx.Tx) error {
		_, err := e.Svc.Status(ctx, tx, uuid.Must(uuid.NewV7()), true)
		return err
	})
	require.ErrorIs(t, err, partner.ErrPartnerNotFound)
}

func TestList_PaginatesAndFilters(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.create(t, input("Alpha Travel"))
	b := e.create(t, input("beta-tours", "scheduled_ride"))
	c := e.create(t, input("gamma_bus"))
	require.NoError(t, e.Svc.Suspend(ctx, e.Admin, b.ID, "tạm dừng"))

	page1, next, err := e.Svc.List(ctx, partner.ListFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, c.ID, page1[0].ID)
	require.Equal(t, b.ID, page1[1].ID)
	require.Equal(t, []string{"scheduled_ride"}, page1[1].ServiceTypes)
	require.Equal(t, b.OwnerUserID, page1[1].OwnerUserID)
	require.NotNil(t, next)

	page2, next, err := e.Svc.List(ctx, partner.ListFilter{Limit: 2, Cursor: next})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, a.ID, page2[0].ID)
	require.Nil(t, next)

	all, next, err := e.Svc.List(ctx, partner.ListFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Nil(t, next)

	suspended := "suspended"
	got, _, err := e.Svc.List(ctx, partner.ListFilter{Status: &suspended})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, b.ID, got[0].ID)

	got, _, err = e.Svc.List(ctx, partner.ListFilter{Query: "TRAVEL"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, a.ID, got[0].ID)

	// Matches legal_name ("Công ty ...") case-insensitively too.
	got, _, err = e.Svc.List(ctx, partner.ListFilter{Query: "công TY"})
	require.NoError(t, err)
	require.Len(t, got, 3)

	// LIKE wildcards in the query are literal.
	got, _, err = e.Svc.List(ctx, partner.ListFilter{Query: "_"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, c.ID, got[0].ID)
	got, _, err = e.Svc.List(ctx, partner.ListFilter{Query: "%"})
	require.NoError(t, err)
	require.Empty(t, got)

	bogus := "deleted"
	_, _, err = e.Svc.List(ctx, partner.ListFilter{Status: &bogus})
	status, _ := appCode(t, err)
	require.Equal(t, http.StatusBadRequest, status)
}
