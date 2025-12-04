package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"code.gitea.io/gitea/models/organization"
	"code.gitea.io/gitea/models/unittest"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setPaywallConfig(t testing.TB, enabled bool, sidecarURL string) func() {
	t.Helper()
	require.NotNil(t, setting.CfgProvider)

	sec := setting.CfgProvider.Section("payments")
	hadEnabled := sec.HasKey("ENABLED")
	hadSidecar := sec.HasKey("SIDECAR_URL")
	oldEnabled := sec.Key("ENABLED").String()
	oldSidecar := sec.Key("SIDECAR_URL").String()

	sec.Key("ENABLED").SetValue(strconv.FormatBool(enabled))
	if sidecarURL != "" {
		sec.Key("SIDECAR_URL").SetValue(sidecarURL)
	}

	return func() {
		if hadEnabled {
			sec.Key("ENABLED").SetValue(oldEnabled)
		} else {
			sec.DeleteKey("ENABLED")
		}
		if hadSidecar {
			sec.Key("SIDECAR_URL").SetValue(oldSidecar)
		} else if sidecarURL != "" {
			sec.DeleteKey("SIDECAR_URL")
		}
	}
}

// TestOrgCreatePaywallButtonDisabledWithoutSubscription verifies the create button is disabled when paywall is on and no payment is completed.
func TestOrgCreatePaywallButtonDisabledWithoutSubscription(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer setPaywallConfig(t, true, "")()

	session := loginUser(t, "user1")
	req := NewRequest(t, "GET", "/org/create")
	resp := session.MakeRequest(t, req, http.StatusOK)

	doc := NewHTMLParser(t, resp.Body)
	createBtn := doc.Find("button.ui.primary.button")
	require.Equal(t, 1, createBtn.Length())
	_, disabled := createBtn.Attr("disabled")
	assert.True(t, disabled)
}

// TestOrgCreatePaywallButtonEnabledAfterCheckout simulates a paid checkout session and expects the create button to be enabled.
func TestOrgCreatePaywallButtonEnabledAfterCheckout(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/billing/session/sess-paid", r.URL.Path)
		_, _ = w.Write([]byte(`{
			"session_id": "sess-paid",
			"status": "complete",
			"payment_status": "paid",
			"subscription_id": "sub_123",
			"customer_id": "cus_123",
			"expires_at": 0
		}`))
	}))
	defer srv.Close()
	defer setPaywallConfig(t, true, srv.URL)()

	session := loginUser(t, "user1")
	req := NewRequest(t, "GET", "/org/create?checkout_session_id=sess-paid")
	resp := session.MakeRequest(t, req, http.StatusOK)

	doc := NewHTMLParser(t, resp.Body)
	createBtn := doc.Find("button.ui.primary.button")
	require.Equal(t, 1, createBtn.Length())
	_, disabled := createBtn.Attr("disabled")
	assert.False(t, disabled)
}

// TestOrgCreatePaywallRequiresBillingToken enforces that org creation is blocked until a billing token/subscription is present.
func TestOrgCreatePaywallRequiresBillingToken(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer setPaywallConfig(t, true, "")()

	session := loginUser(t, "user1")
	req := NewRequest(t, "GET", "/org/create")
	resp := session.MakeRequest(t, req, http.StatusOK)
	doc := NewHTMLParser(t, resp.Body)

	orgName := "paywall-no-token"
	req = NewRequestWithValues(t, "POST", "/org/create", map[string]string{
		"_csrf":                         doc.GetCSRF(),
		"org_name":                      orgName,
		"visibility":                    "0",
		"repo_admin_change_team_access": "true",
	})
	resp = session.MakeRequest(t, req, http.StatusOK)

	assert.Contains(t, resp.Body.String(), "Payment is required to create an organization")
	unittest.AssertNotExistsBean(t, &user_model.User{LowerName: orgName}) // Queries test DB + asserts if User exists or not.
}

// TestOrgCreatePaywallWithBillingTokenCreatesOrgBilling ensures org creation succeeds when a billing token is provided and persists billing linkage.
func TestOrgCreatePaywallWithBillingTokenCreatesOrgBilling(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer setPaywallConfig(t, true, "")()

	session := loginUser(t, "user1")
	req := NewRequest(t, "GET", "/org/create")
	resp := session.MakeRequest(t, req, http.StatusOK)
	doc := NewHTMLParser(t, resp.Body)

	orgName := "paywall-success"
	billingToken := "sess_success"
	req = NewRequestWithValues(t, "POST", "/org/create", map[string]string{
		"_csrf":                         doc.GetCSRF(),
		"org_name":                      orgName,
		"visibility":                    "0",
		"repo_admin_change_team_access": "true",
		"billing_token":                 billingToken,
	})
	resp = session.MakeRequest(t, req, http.StatusSeeOther)
	assert.Equal(t, http.StatusSeeOther, resp.Code)

	org := unittest.AssertExistsAndLoadBean(t, &user_model.User{
		LowerName: strings.ToLower(orgName),
		Type:      user_model.UserTypeOrganization,
	})
	ob, err := organization.GetOrgBilling(t.Context(), org.ID)
	require.NoError(t, err)
	require.NotNil(t, ob)
	assert.Equal(t, billingToken, ob.CheckoutSessionID)
	assert.Equal(t, org.ID, ob.OrgID)
}

// TestSyncSeatsUpdatesStripeQuantity stubs the sidecar to accept seat sync and verifies seat count persistence and sidecar payload.
func TestSyncSeatsUpdatesStripeQuantity(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	ctx := t.Context()
	org := unittest.AssertExistsAndLoadBean(t, &user_model.User{LowerName: "org3"})
	require.NoError(t, organization.UpsertOrgBilling(ctx, &organization.OrgBilling{
		OrgID:          org.ID,
		SubscriptionID: "sub_sync",
		CustomerID:     "cus_sync",
	}))

	memberIDs, err := organization.GetWriteMembersIDs(ctx, org.ID)
	require.NoError(t, err)

	var receivedQuantity int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/billing/subscription/sub_sync":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sub_sync","status":"active","quantity":1,"customer":"cus_sync","current_period_end":0}`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/billing/subscription/sub_sync/quantity":
			payload, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			var body map[string]int
			require.NoError(t, json.Unmarshal(payload, &body))
			receivedQuantity = body["quantity"]
			return
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	defer setPaywallConfig(t, true, srv.URL)()

	session := loginUser(t, "user2")
	req := NewRequest(t, "GET", "/org/org3/settings")
	resp := session.MakeRequest(t, req, http.StatusOK)
	csrf := NewHTMLParser(t, resp.Body).GetCSRF()

	req = NewRequestWithValues(t, "POST", "/org/org3/settings/sync_seats", map[string]string{
		"_csrf": csrf,
	})
	resp = session.MakeRequest(t, req, http.StatusSeeOther)
	assert.Equal(t, http.StatusSeeOther, resp.Code)

	flash := session.GetCookieFlashMessage()
	assert.Contains(t, flash.SuccessMsg, "Synced seats to")

	updated, err := organization.GetOrgBilling(ctx, org.ID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.Equal(t, len(memberIDs), updated.LastSeatCount)
	assert.True(t, updated.LastSync > 0)
	assert.Equal(t, len(memberIDs), receivedQuantity)
}

// TestSyncSeatsWithoutSubscriptionShowsError confirms the sync endpoint reports an error when no subscription is linked.
func TestSyncSeatsWithoutSubscriptionShowsError(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	defer setPaywallConfig(t, true, "")()

	session := loginUser(t, "user2")
	req := NewRequest(t, "GET", "/org/org3/settings")
	resp := session.MakeRequest(t, req, http.StatusOK)
	csrf := NewHTMLParser(t, resp.Body).GetCSRF()

	req = NewRequestWithValues(t, "POST", "/org/org3/settings/sync_seats", map[string]string{
		"_csrf": csrf,
	})
	session.MakeRequest(t, req, http.StatusSeeOther)
	flash := session.GetCookieFlashMessage()
	assert.Equal(t, "No subscription found to sync seats", flash.ErrorMsg)
}

// TestOrgSettingsBillingLinkVisibility checks portal button disable/enable state based on presence of a billing customer.
func TestOrgSettingsBillingLinkVisibility(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	ctx := t.Context()

	org := unittest.AssertExistsAndLoadBean(t, &user_model.User{LowerName: "org3"})
	defer setPaywallConfig(t, true, "")()

	t.Run("DisabledWithoutBilling", func(t *testing.T) {
		session := loginUser(t, "user2")
		req := NewRequest(t, "GET", "/org/org3/settings")
		resp := session.MakeRequest(t, req, http.StatusOK)

		doc := NewHTMLParser(t, resp.Body)
		button := doc.Find(`form[action$="/billing/portal"] button`)
		require.Equal(t, 1, button.Length())
		_, disabled := button.Attr("disabled")
		assert.True(t, disabled)
	})

	t.Run("EnabledWithCustomer", func(t *testing.T) {
		require.NoError(t, organization.UpsertOrgBilling(ctx, &organization.OrgBilling{
			OrgID:         org.ID,
			CustomerID:    "cus_portal",
			LastSync:      0,
			LastSeatCount: 0,
		}))

		session := loginUser(t, "user2")
		req := NewRequest(t, "GET", "/org/org3/settings")
		resp := session.MakeRequest(t, req, http.StatusOK)

		doc := NewHTMLParser(t, resp.Body)
		button := doc.Find(`form[action$="/billing/portal"] button`)
		require.Equal(t, 1, button.Length())
		_, disabled := button.Attr("disabled")
		assert.False(t, disabled)
	})
}

// TestBillingPortalMissingCustomerShowsError ensures portal generation errors are surfaced when no customer id exists.
func TestBillingPortalMissingCustomerShowsError(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	ctx := t.Context()
	org := unittest.AssertExistsAndLoadBean(t, &user_model.User{LowerName: "org3"})
	require.NoError(t, organization.UpsertOrgBilling(ctx, &organization.OrgBilling{OrgID: org.ID}))
	defer setPaywallConfig(t, true, "")()

	session := loginUser(t, "user2")
	req := NewRequest(t, "GET", "/org/org3/settings")
	resp := session.MakeRequest(t, req, http.StatusOK)
	csrf := NewHTMLParser(t, resp.Body).GetCSRF()

	req = NewRequestWithValues(t, "POST", "/org/org3/settings/billing/portal", map[string]string{
		"_csrf": csrf,
	})
	session.MakeRequest(t, req, http.StatusSeeOther)
	flash := session.GetCookieFlashMessage()
	assert.Equal(t, "No billing customer found for this organization", flash.ErrorMsg)
}
