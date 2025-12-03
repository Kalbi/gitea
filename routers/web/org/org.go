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
	// NOTE: ctx.Data is where template fetches data; this block is temporary scaffolding.
	// Eventually push paywall/session state handling into a dedicated helper/service
	// TODO: Persist stripe ids in db instead of template here using xorm

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

	if sessionID != "" {
		status, paid, subID := fetchCheckoutStatus(ctx, sessionID)
		ctx.Data["CheckoutStatus"] = status
		if paid {
			ctx.Data["PaymentCompleted"] = true
			ctx.Data["billing_token"] = sessionID
			ctx.Data["HasActiveSubscription"] = true
			ctx.Data["ActiveSubscriptionID"] = subID
			ctx.Session.Set("subscription_id", subID)
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
	if subID, ok := ctx.Session.Get("subscription_id").(string); ok && subID != "" {
		ctx.Data["HasActiveSubscription"] = true
		ctx.Data["ActiveSubscriptionID"] = subID
	}

	if !ctx.Doer.CanCreateOrganization() {
		ctx.ServerError("Not allowed", errors.New(ctx.Locale.TrString("org.form.create_org_not_allowed")))
		return
	}

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplCreateOrg)
		return
	}

	hasSub := ctx.Data["HasActiveSubscription"].(bool)

	if isPaywallEnabled() && ctx.Req.FormValue("generate_checkout") == "1" {
		checkoutURL, err := createCheckoutForOrg(ctx, &form)
		if err != nil {
			ctx.ServerError("CreateCheckoutSession", err)
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

	ctx.Redirect(org.AsUser().DashboardLink())
}

func isPaywallEnabled() bool {
	return setting.CfgProvider.Section("payments").Key("ENABLED").MustBool(false)
}

func paymentsBaseURL() string {
	return setting.CfgProvider.Section("payments").Key("SIDECAR_URL").MustString("http://payments:9000")
}

