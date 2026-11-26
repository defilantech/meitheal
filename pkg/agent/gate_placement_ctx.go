/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agent

import (
	"context"

	meithealv1alpha1 "github.com/defilantech/meitheal/api/v1alpha1"
)

type gatePlacementKey struct{}

// WithGatePlacement returns a context carrying a gate placement taken from the
// resolved GateProfile. It is the trusted channel from the deterministic
// executor to run_gate_job: the placement never appears in the model-visible
// tool-call JSON, and run_gate_job rejects a `placement` key in its args, so a
// model-driven Agent cannot supply one. A nil placement returns ctx unchanged.
func WithGatePlacement(ctx context.Context, p *meithealv1alpha1.GatePlacement) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, gatePlacementKey{}, p)
}

// GatePlacementFromContext returns the placement set by WithGatePlacement, or
// nil when none was set.
func GatePlacementFromContext(ctx context.Context) *meithealv1alpha1.GatePlacement {
	p, _ := ctx.Value(gatePlacementKey{}).(*meithealv1alpha1.GatePlacement)
	return p
}
