/*
Copyright 2020 the original author or authors.

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

package reconcilers

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ SubReconciler[client.Object] = (*SyncReconciler[client.Object])(nil)

// SyncReconciler is a sub reconciler for custom reconciliation logic. No
// behavior is defined directly.
type SyncReconciler[Type client.Object] struct {
	// Name used to identify this reconciler.  Defaults to `SyncReconciler`.  Ideally unique, but
	// not required to be so.
	//
	// +optional
	Name string

	// Setup performs initialization on the manager and builder this reconciler
	// will run with. It's common to setup field indexes and watch resources.
	//
	// +optional
	Setup func(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error

	// SyncDuringFinalization indicates the Sync method should be called when the resource is pending deletion.
	SyncDuringFinalization bool

	// Sync does whatever work is necessary for the reconciler.
	//
	// If SyncDuringFinalization is true this method is called when the resource is pending
	// deletion. This is useful if the reconciler is managing reference data.
	//
	// Mutually exclusive with SyncWithResult
	Sync func(ctx context.Context, resource Type) error

	// SyncWithResult does whatever work is necessary for the reconciler.
	//
	// If SyncDuringFinalization is true this method is called when the resource is pending
	// deletion. This is useful if the reconciler is managing reference data.
	//
	// Mutually exclusive with Sync
	SyncWithResult func(ctx context.Context, resource Type) (Result, error)

	// Finalize does whatever work is necessary for the reconciler when the resource is pending
	// deletion. If this reconciler sets a finalizer it should do the necessary work to clean up
	// state the finalizer represents and then clear the finalizer.
	//
	// Mutually exclusive with FinalizeWithResult
	//
	// +optional
	Finalize func(ctx context.Context, resource Type) error

	// Finalize does whatever work is necessary for the reconciler when the resource is pending
	// deletion. If this reconciler sets a finalizer it should do the necessary work to clean up
	// state the finalizer represents and then clear the finalizer.
	//
	// Mutually exclusive with Finalize
	//
	// +optional
	FinalizeWithResult func(ctx context.Context, resource Type) (Result, error)

	lazyInit sync.Once
}

func (r *SyncReconciler[T]) SetupWithManager(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error {
	r.init()

	log := logr.FromContextOrDiscard(ctx).
		WithName(r.Name)
	ctx = logr.NewContext(ctx, log)

	if r.Setup == nil {
		return nil
	}
	if err := r.Validate(ctx); err != nil {
		return err
	}
	return r.Setup(ctx, mgr, bldr)
}

func (r *SyncReconciler[T]) init() {
	r.lazyInit.Do(func() {
		if r.Name == "" {
			r.Name = "SyncReconciler"
		}
	})
}

func (r *SyncReconciler[T]) Validate(ctx context.Context) error {
	r.init()

	// validate Sync and SyncWithResult
	if r.Sync == nil && r.SyncWithResult == nil {
		return fmt.Errorf("SyncReconciler %q must implement Sync or SyncWithResult", r.Name)
	}
	if r.Sync != nil && r.SyncWithResult != nil {
		return fmt.Errorf("SyncReconciler %q may not implement both Sync and SyncWithResult", r.Name)
	}

	// validate Finalize and FinalizeWithResult
	if r.Finalize != nil && r.FinalizeWithResult != nil {
		return fmt.Errorf("SyncReconciler %q may not implement both Finalize and FinalizeWithResult", r.Name)
	}

	return nil
}

func (r *SyncReconciler[T]) Reconcile(ctx context.Context, resource T) (Result, error) {
	log := logr.FromContextOrDiscard(ctx).
		WithName(r.Name)
	ctx = logr.NewContext(ctx, log)

	result := Result{}

	if resource.GetDeletionTimestamp() == nil || r.SyncDuringFinalization {
		syncResult, err := r.sync(ctx, resource)
		result = AggregateResults(result, syncResult)
		if err != nil {
			if !errors.Is(err, ErrQuiet) {
				log.Error(err, "unable to sync")
			}
			return result, err
		}
	}

	if resource.GetDeletionTimestamp() != nil {
		finalizeResult, err := r.finalize(ctx, resource)
		result = AggregateResults(result, finalizeResult)
		if err != nil {
			if !errors.Is(err, ErrQuiet) {
				log.Error(err, "unable to finalize")
			}
			return result, err
		}
	}

	return result, nil
}

func (r *SyncReconciler[T]) sync(ctx context.Context, resource T) (Result, error) {
	if r.Sync != nil {
		err := r.Sync(ctx, resource)
		return Result{}, err
	}
	return r.SyncWithResult(ctx, resource)
}

func (r *SyncReconciler[T]) finalize(ctx context.Context, resource T) (Result, error) {
	if r.Finalize != nil {
		err := r.Finalize(ctx, resource)
		return Result{}, err
	}
	if r.FinalizeWithResult != nil {
		return r.FinalizeWithResult(ctx, resource)
	}

	return Result{}, nil
}
