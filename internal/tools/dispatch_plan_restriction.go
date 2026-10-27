package tools

import (
	"context"
	"errors"
	"sync"
)

var planRestrictionConfiguration sync.Mutex

// RestrictPlanBy adds one immutable ancestor Plan restriction before this
// registry is exposed to callers. It shares no registry, approval or execution
// ownership. Enter and Exit both invalidate previously captured child permits.
func (f *DispatchFence) RestrictPlanBy(parent *DispatchFence) error {
	if f == nil || parent == nil {
		return ErrDispatchFenceUnavailable
	}
	planRestrictionConfiguration.Lock()
	defer planRestrictionConfiguration.Unlock()
	if previous := f.planRestriction.Load(); previous != nil {
		if previous == parent {
			return nil
		}
		return errors.New("tools: parent Plan restriction is immutable")
	}
	for ancestor := parent; ancestor != nil; ancestor = ancestor.planRestriction.Load() {
		if ancestor == f {
			return errors.New("tools: cyclic parent Plan restriction")
		}
	}
	f.planRestriction.Store(parent)
	return nil
}

func (f *DispatchFence) withPlanRestriction(callback func(uint64) error) error {
	if parent := f.planRestriction.Load(); parent != nil {
		return parent.WithSnapshot(func(snapshot DispatchFenceSnapshot) error { return callback(snapshot.Plan + snapshot.PlanRestriction) })
	}
	return callback(0)
}

func (f *DispatchFence) snapshotWithParentPlan(parentPlan uint64) DispatchFenceSnapshot {
	snapshot := f.generations
	snapshot.PlanRestriction = parentPlan
	return snapshot
}

type preparedEffectStartKey struct{}
type preparedEffectStartGuard func(func() error) error

func (p *preparedInvocation) withEffectStartGuard(ctx context.Context) context.Context {
	if p.fence.planRestriction.Load() == nil {
		return ctx
	}
	guard := preparedEffectStartGuard(func(start func() error) error {
		check := func() error {
			return p.fence.WithSnapshot(func(current DispatchFenceSnapshot) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				if current != p.generations {
					return ErrDispatchFenceChanged
				}
				return start()
			})
		}
		if p.projectTrustBinding != nil {
			return p.projectTrustBinding.CommitIfCurrent(ctx, check)
		}
		return check()
	})
	return context.WithValue(ctx, preparedEffectStartKey{}, guard)
}

// StartPreparedEffect authorizes only the short final start/mutation callback
// of a captured child invocation. Host hook guards use this same boundary to
// avoid recursively taking its ancestor Plan locks. false means no such
// captured boundary exists and the caller must apply its ordinary policy.
func StartPreparedEffect(ctx context.Context, start func() error) (bool, error) {
	if ctx == nil {
		return false, nil
	}
	guard, ok := ctx.Value(preparedEffectStartKey{}).(preparedEffectStartGuard)
	if !ok {
		return false, nil
	}
	if start == nil {
		return true, errors.New("tools: prepared effect start is nil")
	}
	return true, guard(start)
}

func startPreparedEffect(ctx context.Context, start func() error) error {
	if guarded, err := StartPreparedEffect(ctx, start); guarded {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return start()
}
