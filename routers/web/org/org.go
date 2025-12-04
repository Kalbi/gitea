// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2018 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package org

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/organization"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/templates"
	"code.gitea.io/gitea/modules/web"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/forms"
)

const (
	// tplCreateOrg template path for create organization
	tplCreateOrg templates.TplName = "org/create"
)

// Create render the page for create organization
func Create(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("new_org")
	if !ctx.Doer.CanCreateOrganization() {
		ctx.ServerError("Not allowed", errors.New(ctx.Locale.TrString("org.form.create_org_not_allowed")))
		return
	}

	ctx.Data["visibility"] = setting.Service.DefaultOrgVisibilityMode
	ctx.Data["repo_admin_change_team_access"] = true
	ctx.Data["PaywallEnabled"] = isPaywallEnabled()
	ctx.Data["billing_token"] = ""
	ctx.Data["org_name"] = ""
	ctx.Data["PaymentCompleted"] = false
	ctx.Data["CheckoutStatus"] = ""
	ctx.Data["CheckoutURL"] = ""
	ctx.Data["HasActiveSubscription"] = false
	ctx.Data["ActiveSubscriptionID"] = ""
	ctx.Data["ActiveCustomerID"] = ""
	// NOTE: ctx.Data is where template fetches data; this block is temporary scaffolding.
	// Eventually push paywall/session state handling into a dedicated helper/service
	// TODO: add a pending checkout table keyed by CheckoutSessionID (org_name/customer/subscription)
	// and transfer to OrgBilling after org creation to reduce reliance on session

	// reuse session if present, allow resume
	sessionID := ctx.FormString("checkout_session_id")
	if sessionID == "" {
		if val := ctx.Session.Get("checkout_session_id"); val != nil {
			if sid, ok := val.(string); ok {
				sessionID = sid
			}
		}
	}
	if checkoutURL, ok := ctx.Session.Get("checkout_url").(string); ok {
		ctx.Data["CheckoutURL"] = checkoutURL
	}
	if subID, ok := ctx.Session.Get("subscription_id").(string); ok && subID != "" {
		ctx.Data["HasActiveSubscription"] = true
		ctx.Data["ActiveSubscriptionID"] = subID
	}
	if custID, ok := ctx.Session.Get("customer_id").(string); ok && custID != "" {
		ctx.Data["ActiveCustomerID"] = custID
	}

	if sessionID != "" {
		status, paid, subID, custID := fetchCheckoutStatus(ctx, sessionID)
		ctx.Data["CheckoutStatus"] = status
		if paid {
			ctx.Data["PaymentCompleted"] = true
			ctx.Data["billing_token"] = sessionID
			ctx.Data["HasActiveSubscription"] = true
			ctx.Data["ActiveSubscriptionID"] = subID
			ctx.Session.Set("subscription_id", subID)
			if custID != "" {
				ctx.Data["ActiveCustomerID"] = custID
				ctx.Session.Set("customer_id", custID)
			}
		} else {
			ctx.Data["billing_token"] = sessionID
		}
	}
	if orgName := ctx.FormString("org_name"); orgName != "" {
		ctx.Data["org_name"] = orgName
	}

	ctx.HTML(http.StatusOK, tplCreateOrg)
}

// CreatePost response for create organization
func CreatePost(ctx *context.Context) {
	form := *web.GetForm(ctx).(*forms.CreateOrgForm)
	ctx.Data["Title"] = ctx.Tr("new_org")
	ctx.Data["PaywallEnabled"] = isPaywallEnabled()
	ctx.Data["billing_token"] = form.BillingToken
	ctx.Data["org_name"] = form.OrgName
	ctx.Data["visibility"] = form.Visibility
	ctx.Data["repo_admin_change_team_access"] = form.RepoAdminChangeTeamAccess
	ctx.Data["ActiveCustomerID"] = ""
	if subID, ok := ctx.Session.Get("subscription_id").(string); ok && subID != "" {
		ctx.Data["HasActiveSubscription"] = true
		ctx.Data["ActiveSubscriptionID"] = subID
	}
	if custID, ok := ctx.Session.Get("customer_id").(string); ok && custID != "" {
		ctx.Data["ActiveCustomerID"] = custID
	}

	if !ctx.Doer.CanCreateOrganization() {
		ctx.ServerError("Not allowed", errors.New(ctx.Locale.TrString("org.form.create_org_not_allowed")))
		return
	}

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplCreateOrg)
		return
	}

	hasSub := false
	if v, ok := ctx.Data["HasActiveSubscription"].(bool); ok {
		hasSub = v
	}

	if isPaywallEnabled() && ctx.Req.FormValue("generate_checkout") == "1" {
		checkoutURL, err := createCheckoutForOrg(ctx, &form)
		if err != nil {
			log.Warn("CreateCheckoutSession failed: %v", err)
			ctx.Flash.Error(ctx.Tr("org.form.payment_generate_failed"))
			ctx.Redirect(ctx.Link)
			return
		}
		ctx.Redirect(checkoutURL)
		return
	}

	if isPaywallEnabled() && !hasSub && form.BillingToken == "" {
		ctx.Data["Err_BillingToken"] = true
		msg := ctx.Tr("org.form.payment_required")
		ctx.RenderWithErr(msg, tplCreateOrg, &form)
		return
	}

	org := &organization.Organization{
		Name:                      form.OrgName,
		IsActive:                  true,
		Type:                      user_model.UserTypeOrganization,
		Visibility:                form.Visibility,
		RepoAdminChangeTeamAccess: form.RepoAdminChangeTeamAccess,
	}

	if err := organization.CreateOrganization(ctx, org, ctx.Doer); err != nil {
		ctx.Data["Err_OrgName"] = true
		switch {
		case user_model.IsErrUserAlreadyExist(err):
			ctx.RenderWithErr(ctx.Tr("form.org_name_been_taken"), tplCreateOrg, &form)
		case db.IsErrNameReserved(err):
			ctx.RenderWithErr(ctx.Tr("org.form.name_reserved", err.(db.ErrNameReserved).Name), tplCreateOrg, &form)
		case db.IsErrNamePatternNotAllowed(err):
			ctx.RenderWithErr(ctx.Tr("org.form.name_pattern_not_allowed", err.(db.ErrNamePatternNotAllowed).Pattern), tplCreateOrg, &form)
		case organization.IsErrUserNotAllowedCreateOrg(err):
			ctx.RenderWithErr(ctx.Tr("org.form.create_org_not_allowed"), tplCreateOrg, &form)
		default:
			ctx.ServerError("CreateOrganization", err)
		}
		return
	}
	log.Trace("Organization created: %s", org.Name)

	// Persist billing linkage if present
	if isPaywallEnabled() {
		subID, _ := ctx.Data["ActiveSubscriptionID"].(string)
		sessionID, _ := ctx.Data["billing_token"].(string)
		custID, _ := ctx.Data["ActiveCustomerID"].(string)
		if subID != "" || sessionID != "" {
			_ = organization.UpsertOrgBilling(ctx, &organization.OrgBilling{
				OrgID:             org.ID,
				SubscriptionID:    subID,
				CustomerID:        custID,
				CheckoutSessionID: sessionID,
			})
		}
	}

	ctx.Redirect(org.AsUser().DashboardLink())
}

func isPaywallEnabled() bool {
	return setting.CfgProvider.Section("payments").Key("ENABLED").MustBool(false)
}

func paymentsBaseURL() string {
	return setting.CfgProvider.Section("payments").Key("SIDECAR_URL").MustString("http://payments:9000")
}

type checkoutResponse struct {
	CheckoutURL    string `json:"checkout_url"`
	SessionID      string `json:"session_id"`
	CustomerID     string `json:"customer_id"`
	SubscriptionID string `json:"subscription_id"`
	ExpiresAt      int64  `json:"expires_at"`
}

type checkoutStatus struct {
	SessionID      string `json:"session_id"`
	Status         string `json:"status"`
	CustomerID     string `json:"customer_id"`
	SubscriptionID string `json:"subscription_id"`
	PaymentStatus  string `json:"payment_status"`
	ExpiresAt      int64  `json:"expires_at"`
}

func fetchCheckoutStatus(ctx *context.Context, sessionID string) (string, bool, string) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/billing/session/%s", paymentsBaseURL(), url.PathEscape(sessionID)), nil)
	if err != nil {
		log.Warn("checkout status request build failed: %v", err)
		return err.Error(), false, ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Warn("checkout status request error: %v", err)
		return err.Error(), false, ""
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		log.Warn("checkout status http error: %s", resp.Status)
		return fmt.Sprintf("status %s", resp.Status), false, ""
	}

	var st checkoutStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		log.Warn("checkout status decode error: %v", err)
		return err.Error(), false, ""
	}
	paid := st.Status == "complete" || st.PaymentStatus == "paid"
	log.Info("checkout status: paid=%v session=%s status=%s payment_status=%s customer=%s subscription=%s", paid, st.SessionID, st.Status, st.PaymentStatus, st.CustomerID, st.SubscriptionID)
	return fmt.Sprintf("%s/%s", st.Status, st.PaymentStatus), paid, st.SubscriptionID
}

func createCheckoutForOrg(ctx *context.Context, form *forms.CreateOrgForm) (string, error) {
	// TODO: extract payments client/helpers to a separate package (e.g., services/payments) to avoid bloating org handlers.
	payload := map[string]any{
		"org_id":   form.OrgName,
		"org_name": form.OrgName,
		"quantity": 1,
		"success_url": fmt.Sprintf(
			"%sorg/create?org_name=%s&checkout_session_id={CHECKOUT_SESSION_ID}",
			setting.AppURL, url.QueryEscape(form.OrgName),
		),
		"cancel_url": fmt.Sprintf(
			"%sorg/create?org_name=%s&checkout_session_id={CHECKOUT_SESSION_ID}&canceled=1",
			setting.AppURL, url.QueryEscape(form.OrgName),
		),
	}

	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/billing/org/%s/checkout", paymentsBaseURL(), url.PathEscape(form.OrgName)), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("checkout request failed: %s", resp.Status)
	}

	var out checkoutResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.CheckoutURL == "" {
		return "", fmt.Errorf("missing checkout_url in response")
	}
	// Prefill billing token for return flows
	ctx.Session.Set("checkout_session_id", out.SessionID)
	ctx.Session.Set("checkout_url", out.CheckoutURL)
	if out.CustomerID != "" {
		ctx.Session.Set("customer_id", out.CustomerID)
	}
	ctx.Data["billing_token"] = out.SessionID
	return out.CheckoutURL, nil
}
