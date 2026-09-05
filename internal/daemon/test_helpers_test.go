package daemon

import (
	"context"

	"github.com/lxdb/bsbctl/internal/config"
)

type SecretResolverFunc func(context.Context, string) (string, error)

func (f SecretResolverFunc) Resolve(ctx context.Context, reference string) (string, error) {
	return f(ctx, reference)
}

func BuildPlan(ctx context.Context, document config.Document, resolver SecretResolver) (ReconciliationPlan, error) {
	return buildPlan(ctx, document, resolver, nil, nil)
}
