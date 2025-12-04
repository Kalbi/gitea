// Copyright 2014 The Gogs Authors. All rights reserved.
// Copyright 2019 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package org

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/models/organization"
	packages_model "code.gitea.io/gitea/models/packages"
	repo_model "code.gitea.io/gitea/models/repo"
	user_model "code.gitea.io/gitea/models/user"
	"code.gitea.io/gitea/models/webhook"
	"code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/optional"
	repo_module "code.gitea.io/gitea/modules/repository"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/modules/structs"
	"code.gitea.io/gitea/modules/timeutil"
	"code.gitea.io/gitea/modules/templates"
	"code.gitea.io/gitea/modules/util"
	"code.gitea.io/gitea/modules/web"
	shared_user "code.gitea.io/gitea/routers/web/shared/user"
	user_setting "code.gitea.io/gitea/routers/web/user/setting"
	"code.gitea.io/gitea/services/context"
	"code.gitea.io/gitea/services/forms"
	org_service "code.gitea.io/gitea/services/org"
	user_service "code.gitea.io/gitea/services/user"
)

const (
	// tplSettingsOptions template path for render settings
	tplSettingsOptions templates.TplName = "org/settings/options"
	// tplSettingsHooks template path for render hook settings
	tplSettingsHooks templates.TplName = "org/settings/hooks"
	// tplSettingsLabels template path for render labels settings
	tplSettingsLabels templates.TplName = "org/settings/labels"
)

type subscriptionSummary struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	Quantity         int    `json:"quantity"`
	Customer         string `json:"customer"`
	CurrentPeriodEnd int64  `json:"current_period_end"`
}

// Settings render the main settings page
func Settings(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("org.settings")
	ctx.Data["PageIsOrgSettings"] = true
	ctx.Data["PageIsSettingsOptions"] = true
	ctx.Data["CurrentVisibility"] = ctx.Org.Organization.Visibility
	ctx.Data["RepoAdminChangeTeamAccess"] = ctx.Org.Organization.RepoAdminChangeTeamAccess
	ctx.Data["ContextUser"] = ctx.ContextUser

	loadOrgBillingSummary(ctx)

	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}

	ctx.HTML(http.StatusOK, tplSettingsOptions)
}

// SettingsPost response for settings change submitted
func SettingsPost(ctx *context.Context) {
	loadOrgBillingSummary(ctx)
	form := web.GetForm(ctx).(*forms.UpdateOrgSettingForm)
	ctx.Data["Title"] = ctx.Tr("org.settings")
	ctx.Data["PageIsOrgSettings"] = true
	ctx.Data["PageIsSettingsOptions"] = true
	ctx.Data["CurrentVisibility"] = ctx.Org.Organization.Visibility

	if ctx.HasError() {
		ctx.HTML(http.StatusOK, tplSettingsOptions)
		return
	}

	org := ctx.Org.Organization

	if form.Email != "" {
		if err := user_service.ReplacePrimaryEmailAddress(ctx, org.AsUser(), form.Email); err != nil {
			ctx.Data["Err_Email"] = true
			ctx.RenderWithErr(ctx.Tr("form.email_invalid"), tplSettingsOptions, &form)
			return
		}
	}

	opts := &user_service.UpdateOptions{
		FullName:                  optional.Some(form.FullName),
		Description:               optional.Some(form.Description),
		Website:                   optional.Some(form.Website),
		Location:                  optional.Some(form.Location),
		RepoAdminChangeTeamAccess: optional.Some(form.RepoAdminChangeTeamAccess),
	}
	if ctx.Doer.IsAdmin {
		opts.MaxRepoCreation = optional.Some(form.MaxRepoCreation)
	}

	if err := user_service.UpdateUser(ctx, org.AsUser(), opts); err != nil {
		ctx.ServerError("UpdateUser", err)
		return
	}

	log.Trace("Organization setting updated: %s", org.Name)
	ctx.Flash.Success(ctx.Tr("org.settings.update_setting_success"))
	ctx.Redirect(ctx.Org.OrgLink + "/settings")
}

// BillingPortal generates a Stripe customer portal URL and redirects the org owner there.
func BillingPortal(ctx *context.Context) {
	org := ctx.Org.Organization

	ob, err := organization.GetOrgBilling(ctx, org.ID)
	if err != nil {
		ctx.ServerError("GetOrgBilling", err)
		return
	}
	if ob == nil || ob.CustomerID == "" {
		ctx.Flash.Error("No billing customer found for this organization")
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	customerID := ob.CustomerID
	if customerID == "" && ob.SubscriptionID != "" {
		if sub, err := fetchSubscription(ctx, ob.SubscriptionID); err == nil && sub != nil && sub.Customer != "" {
			customerID = sub.Customer
			ob.CustomerID = sub.Customer
			_ = organization.UpsertOrgBilling(ctx, ob)
		}
	}
	if customerID == "" {
		ctx.Flash.Error("No billing customer found for this organization")
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	payload := map[string]string{
		"customer_id": customerID,
		"return_url":  fmt.Sprintf("%sorg/%s/settings", setting.AppURL, url.PathEscape(org.Name)),
	}
	if ob.SubscriptionID != "" {
		payload["subscription_id"] = ob.SubscriptionID
	}

	body, err := json.Marshal(payload)
	if err != nil {
		ctx.ServerError("MarshalPortalPayload", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/billing/portal", paymentsBaseURL()), bytes.NewReader(body))
	if err != nil {
		ctx.ServerError("BillingPortalRequest", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ctx.Flash.Error(fmt.Sprintf("Failed to generate billing portal: %v", err))
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		ctx.Flash.Error(fmt.Sprintf("Failed to generate billing portal: %s", resp.Status))
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	var out struct {
		PortalURL string `json:"portal_url"`
		ID        string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		ctx.Flash.Error(fmt.Sprintf("Failed to parse billing portal response: %v", err))
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}
	if out.PortalURL == "" {
		ctx.Flash.Error("Failed to generate billing portal: missing URL")
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	ctx.Redirect(out.PortalURL)
}

// SyncSeats recomputes billable seats for the org and pushes the quantity to the payments sidecar.
func SyncSeats(ctx *context.Context) {
	org := ctx.Org.Organization

	ob, err := organization.GetOrgBilling(ctx, org.ID)
	if err != nil {
		ctx.ServerError("GetOrgBilling", err)
		return
	}
	if ob == nil || ob.SubscriptionID == "" {
		ctx.Flash.Error("No subscription found to sync seats")
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	memberIDs, err := organization.GetWriteMembersIDs(ctx, org.ID)
	if err != nil {
		log.Error("GetWriteMembersIDs org_id=%d: %v", org.ID, err)
		ctx.Flash.Error("Failed to compute seat count")
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}
	seatCount := len(memberIDs)

	body, err := json.Marshal(map[string]int{"quantity": seatCount})
	if err != nil {
		ctx.ServerError("MarshalSyncSeatsPayload", err)
		return
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/billing/subscription/%s/quantity", paymentsBaseURL(), url.PathEscape(ob.SubscriptionID)), bytes.NewReader(body))
	if err != nil {
		ctx.ServerError("SyncSeatsRequest", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ctx.Flash.Error(fmt.Sprintf("Failed to sync seats: %v", err))
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		ctx.Flash.Error(fmt.Sprintf("Failed to sync seats: %s", resp.Status))
		ctx.Redirect(ctx.Org.OrgLink + "/settings")
		return
	}

	ob.LastSeatCount = seatCount
	ob.LastSync = timeutil.TimeStampNow()
	if err := organization.UpsertOrgBilling(ctx, ob); err != nil {
		log.Warn("UpsertOrgBilling sync seats org_id=%d: %v", org.ID, err)
	}

	ctx.Flash.Success(fmt.Sprintf("Synced seats to %d", seatCount))
	ctx.Redirect(ctx.Org.OrgLink + "/settings")
}

func fetchSubscription(ctx *context.Context, subscriptionID string) (*subscriptionSummary, error) {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/billing/subscription/%s", paymentsBaseURL(), url.PathEscape(subscriptionID)), nil)
	if err != nil {
		return nil, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("status %s", resp.Status)
	}

	var out subscriptionSummary
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// loadOrgBillingSummary populates billing-related data for the settings template.
func loadOrgBillingSummary(ctx *context.Context) {
	ctx.Data["OrgBilling"] = (*organization.OrgBilling)(nil)
	ctx.Data["BillingSeatCount"] = 0
	ctx.Data["BillingSubscription"] = nil
	ctx.Data["BillingCustomerID"] = ""
	if !isPaywallEnabled() {
		return
	}

	ob, err := organization.GetOrgBilling(ctx, ctx.Org.Organization.ID)
	if err != nil {
		log.Warn("GetOrgBilling org_id=%d: %v", ctx.Org.Organization.ID, err)
		return
	}
	if ob == nil {
		return
	}
	ctx.Data["OrgBilling"] = ob
	ctx.Data["BillingCustomerID"] = ob.CustomerID

	memberIDs, err := organization.GetWriteMembersIDs(ctx, ctx.Org.Organization.ID)
	if err != nil {
		log.Warn("GetWriteMembersIDs org_id=%d: %v", ctx.Org.Organization.ID, err)
	} else {
		ctx.Data["BillingSeatCount"] = len(memberIDs)
	}

	if ob.SubscriptionID != "" {
		if sub, err := fetchSubscription(ctx, ob.SubscriptionID); err != nil {
			log.Warn("fetchSubscription org_id=%d sub_id=%s: %v", ctx.Org.Organization.ID, ob.SubscriptionID, err)
		} else {
			ctx.Data["BillingSubscription"] = sub
			if ctx.Data["BillingCustomerID"] == "" && sub.Customer != "" {
				ctx.Data["BillingCustomerID"] = sub.Customer
			}
		}
	}
}

// SettingsAvatar response for change avatar on settings page
func SettingsAvatar(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.AvatarForm)
	form.Source = forms.AvatarLocal
	if err := user_setting.UpdateAvatarSetting(ctx, form, ctx.Org.Organization.AsUser()); err != nil {
		ctx.Flash.Error(err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("org.settings.update_avatar_success"))
	}

	ctx.Redirect(ctx.Org.OrgLink + "/settings")
}

// SettingsDeleteAvatar response for delete avatar on settings page
func SettingsDeleteAvatar(ctx *context.Context) {
	if err := user_service.DeleteAvatar(ctx, ctx.Org.Organization.AsUser()); err != nil {
		ctx.Flash.Error(err.Error())
	}

	ctx.JSONRedirect(ctx.Org.OrgLink + "/settings")
}

// SettingsDeleteOrgPost response for deleting an organization
func SettingsDeleteOrgPost(ctx *context.Context) {
	if ctx.Org.Organization.Name != ctx.FormString("org_name") {
		ctx.JSONError(ctx.Tr("form.enterred_invalid_org_name"))
		return
	}

	if err := org_service.DeleteOrganization(ctx, ctx.Org.Organization, false /* no purge */); err != nil {
		if repo_model.IsErrUserOwnRepos(err) {
			ctx.JSONError(ctx.Tr("form.org_still_own_repo"))
		} else if packages_model.IsErrUserOwnPackages(err) {
			ctx.JSONError(ctx.Tr("form.org_still_own_packages"))
		} else {
			log.Error("DeleteOrganization: %v", err)
			ctx.JSONError(util.Iif(ctx.Doer.IsAdmin, err.Error(), string(ctx.Tr("org.settings.delete_failed"))))
		}
		return
	}

	ctx.Flash.Success(ctx.Tr("org.settings.delete_successful", ctx.Org.Organization.Name))
	ctx.JSONRedirect(setting.AppSubURL + "/")
}

// Webhooks render webhook list page
func Webhooks(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("org.settings")
	ctx.Data["PageIsOrgSettings"] = true
	ctx.Data["PageIsSettingsHooks"] = true
	ctx.Data["BaseLink"] = ctx.Org.OrgLink + "/settings/hooks"
	ctx.Data["BaseLinkNew"] = ctx.Org.OrgLink + "/settings/hooks"
	ctx.Data["Description"] = ctx.Tr("org.settings.hooks_desc")

	ws, err := db.Find[webhook.Webhook](ctx, webhook.ListWebhookOptions{OwnerID: ctx.Org.Organization.ID})
	if err != nil {
		ctx.ServerError("ListWebhooksByOpts", err)
		return
	}

	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}

	ctx.Data["Webhooks"] = ws
	ctx.HTML(http.StatusOK, tplSettingsHooks)
}

// DeleteWebhook response for delete webhook
func DeleteWebhook(ctx *context.Context) {
	if err := webhook.DeleteWebhookByOwnerID(ctx, ctx.Org.Organization.ID, ctx.FormInt64("id")); err != nil {
		ctx.Flash.Error("DeleteWebhookByOwnerID: " + err.Error())
	} else {
		ctx.Flash.Success(ctx.Tr("repo.settings.webhook_deletion_success"))
	}

	ctx.JSONRedirect(ctx.Org.OrgLink + "/settings/hooks")
}

// Labels render organization labels page
func Labels(ctx *context.Context) {
	ctx.Data["Title"] = ctx.Tr("repo.labels")
	ctx.Data["PageIsOrgSettings"] = true
	ctx.Data["PageIsOrgSettingsLabels"] = true
	ctx.Data["LabelTemplateFiles"] = repo_module.LabelTemplateFiles

	if _, err := shared_user.RenderUserOrgHeader(ctx); err != nil {
		ctx.ServerError("RenderUserOrgHeader", err)
		return
	}

	ctx.HTML(http.StatusOK, tplSettingsLabels)
}

// SettingsRenamePost response for renaming organization
func SettingsRenamePost(ctx *context.Context) {
	form := web.GetForm(ctx).(*forms.RenameOrgForm)
	if ctx.HasError() {
		ctx.JSONError(ctx.GetErrMsg())
		return
	}

	oldOrgName, newOrgName := ctx.Org.Organization.Name, form.NewOrgName

	if form.OrgName != oldOrgName {
		ctx.JSONError(ctx.Tr("form.enterred_invalid_org_name"))
		return
	}
	if newOrgName == oldOrgName {
		ctx.JSONError(ctx.Tr("org.settings.rename_no_change"))
		return
	}

	if err := user_service.RenameUser(ctx, ctx.Org.Organization.AsUser(), newOrgName, ctx.Doer); err != nil {
		if user_model.IsErrUserAlreadyExist(err) {
			ctx.JSONError(ctx.Tr("org.form.name_been_taken", newOrgName))
		} else if db.IsErrNameReserved(err) {
			ctx.JSONError(ctx.Tr("org.form.name_reserved", newOrgName))
		} else if db.IsErrNamePatternNotAllowed(err) {
			ctx.JSONError(ctx.Tr("org.form.name_pattern_not_allowed", newOrgName))
		} else {
			log.Error("RenameOrganization: %v", err)
			ctx.JSONError(util.Iif(ctx.Doer.IsAdmin, err.Error(), string(ctx.Tr("org.settings.rename_failed"))))
		}
		return
	}

	ctx.Flash.Success(ctx.Tr("org.settings.rename_success", oldOrgName, newOrgName))
	ctx.JSONRedirect(setting.AppSubURL + "/org/" + url.PathEscape(newOrgName) + "/settings")
}

// SettingsChangeVisibilityPost response for change organization visibility
func SettingsChangeVisibilityPost(ctx *context.Context) {
	visibility, ok := structs.VisibilityModes[ctx.FormString("visibility")]
	if !ok {
		ctx.Flash.Error(ctx.Tr("invalid_data", visibility))
		ctx.JSONRedirect(setting.AppSubURL + "/org/" + url.PathEscape(ctx.Org.Organization.Name) + "/settings")
		return
	}

	if ctx.Org.Organization.Visibility == visibility {
		ctx.Flash.Info(ctx.Tr("nothing_has_been_changed"))
		ctx.JSONRedirect(setting.AppSubURL + "/org/" + url.PathEscape(ctx.Org.Organization.Name) + "/settings")
		return
	}

	if err := org_service.ChangeOrganizationVisibility(ctx, ctx.Org.Organization, visibility); err != nil {
		log.Error("ChangeOrganizationVisibility: %v", err)
		ctx.JSONError(ctx.Tr("error.occurred"))
		return
	}

	ctx.Flash.Success(ctx.Tr("org.settings.change_visibility_success", ctx.Org.Organization.Name))
	ctx.JSONRedirect(setting.AppSubURL + "/org/" + url.PathEscape(ctx.Org.Organization.Name) + "/settings")
}
