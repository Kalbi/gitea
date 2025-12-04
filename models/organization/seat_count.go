package organization

import (
	"context"

	"code.gitea.io/gitea/models/perm"
	"code.gitea.io/gitea/models/unit"
	"code.gitea.io/gitea/modules/log"
)

// GetWriteMembersIDs returns unique user IDs who can write/create repos in the org:
// - Owner team members
// - Teams with write/admin access
// - Teams with write/create permissions on the Code unit
// - Teams allowed to create org repos
func GetWriteMembersIDs(ctx context.Context, orgID int64) ([]int64, error) {
	teams, err := FindOrgTeams(ctx, orgID)
	if err != nil {
		return nil, err
	}

	ids := make(map[int64]struct{})
	for _, team := range teams {
		include := team.IsOwnerTeam() || team.CanCreateOrgRepo || team.AccessMode >= perm.AccessModeWrite
		if !include {
			if access := team.UnitAccessMode(ctx, unit.TypeCode); access >= perm.AccessModeWrite {
				include = true
			}
		}
		if !include {
			continue
		}

		members, err := GetTeamMembers(ctx, &SearchMembersOptions{TeamID: team.ID})
		if err != nil {
			log.Warn("GetTeamMembers org_id=%d team_id=%d: %v", orgID, team.ID, err)
			return nil, err
		}
		for _, m := range members {
			ids[m.ID] = struct{}{}
		}
	}

	result := make([]int64, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result, nil
}
