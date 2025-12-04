// SPDX-License-Identifier: MIT

package v1_26

import (
	"code.gitea.io/gitea/modules/timeutil"
	"xorm.io/xorm"
)

type orgBilling struct {
	OrgID             int64  `xorm:"pk"`
	SubscriptionID    string `xorm:"INDEX"`
	CustomerID        string
	CheckoutSessionID string
	LastSeatCount     int
	LastSync          timeutil.TimeStamp `xorm:"INDEX"`
}

func init() {
	Register(Migration{
		Title:   "add org billing table",
		Version: 324,
		Migrate: func(x *xorm.Engine) error {
			return x.Sync(new(orgBilling))
		},
	})
}
