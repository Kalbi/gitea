// Copyright 2025 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package organization

import (
	"context"

	"code.gitea.io/gitea/models/db"
	"code.gitea.io/gitea/modules/timeutil"
)

// OrgBilling stores Stripe billing identifiers per org.
type OrgBilling struct {
	OrgID             int64  `xorm:"pk"`
	SubscriptionID    string `xorm:"INDEX"`
	CustomerID        string
	CheckoutSessionID string
	LastSeatCount     int
	LastSync          timeutil.TimeStamp `xorm:"INDEX"`
	CreatedUnix       timeutil.TimeStamp `xorm:"created"`
	UpdatedUnix       timeutil.TimeStamp `xorm:"updated"`
}

func init() {
	db.RegisterModel(new(OrgBilling))
}

// GetOrgBilling fetches billing info for an org, or nil if none exists.
func GetOrgBilling(ctx context.Context, orgID int64) (*OrgBilling, error) {
	ob := new(OrgBilling)
	has, err := db.GetEngine(ctx).ID(orgID).Get(ob)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, nil
	}
	return ob, nil
}

// UpsertOrgBilling inserts or updates billing info for an org.
func UpsertOrgBilling(ctx context.Context, ob *OrgBilling) error {
	if ob == nil || ob.OrgID == 0 {
		return nil
	}
	e := db.GetEngine(ctx)
	has, err := e.ID(ob.OrgID).Exist(new(OrgBilling))
	if err != nil {
		return err
	}
	if has {
		_, err = e.ID(ob.OrgID).Cols(
			"subscription_id",
			"customer_id",
			"checkout_session_id",
			"last_seat_count",
			"last_sync",
		).Update(ob)
		return err
	}
	_, err = e.Insert(ob)
	return err
}
