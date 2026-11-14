// Package identity describes the end user on whose behalf the gateway calls an agent.
package identity

import "context"

// User is the channel user behind a turn. Every field comes from a validated channel
// request (for Teams: a Bot Connector activity whose token was verified).
type User struct {
	// ID is the stable directory identity (Entra object id for Teams).
	ID string
	// ChannelUserID is the channel-specific id (Teams "29:..." id).
	ChannelUserID string
	Name          string
	TenantID      string
}

// Field returns a named attribute, used for identity headers and JWT subjects.
func (u User) Field(name string) string {
	switch name {
	case "aadObjectId":
		return u.ID
	case "teamsUserId":
		return u.ChannelUserID
	case "userName":
		return u.Name
	case "tenantId":
		return u.TenantID
	}
	return ""
}

type ctxKey struct{}

func With(ctx context.Context, u User) context.Context { return context.WithValue(ctx, ctxKey{}, u) }

func From(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}
