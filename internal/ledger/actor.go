package ledger

import "context"

type actorCtxKey struct{}

// WithActor records the acting principal (an API-key id) on ctx so the money
// services can attribute a hold, an approval decision, and every audit event
// they write to the individual key that acted rather than to the whole tenant.
// The API and gRPC layers set this from the resolved key (auth.PrincipalID);
// background paths (the pending sweep, the demo seeder) leave it unset, and the
// readers below fall back to the tenant id so those paths behave exactly as
// before. An empty actor is treated as "unset" so a caller can pass
// auth.PrincipalID(ctx) unconditionally without special-casing the no-key case.
func WithActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, actorCtxKey{}, actor)
}

// actorOr returns the principal recorded by WithActor, or fallback (the tenant
// id) when none is set. Used to stamp the audit Actor and a held pending's
// CreatedBy: with a real principal the four-eyes control compares two distinct
// keys; without one (a background path) it degrades to the tenant, unchanged.
func actorOr(ctx context.Context, fallback string) string {
	if v, ok := ctx.Value(actorCtxKey{}).(string); ok && v != "" {
		return v
	}
	return fallback
}
