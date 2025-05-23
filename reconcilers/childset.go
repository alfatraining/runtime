/*
Copyright 2023 the original author or authors.

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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"reconciler.io/runtime/internal"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	_ SubReconciler[client.Object] = (*ChildSetReconciler[client.Object, client.Object, client.ObjectList])(nil)
)

// ChildSetReconciler is a sub reconciler that manages a set of child resources for a reconciled
// resource. A correlation ID is used to track the desired state of each child resource across
// reconcile requests. A ChildReconciler is created dynamically and reconciled for each desired
// and discovered child resource.
//
// During setup, the child resource type is registered to watch for changes.
type ChildSetReconciler[Type, ChildType client.Object, ChildListType client.ObjectList] struct {
	// Name used to identify this reconciler.  Defaults to `{ChildType}ChildSetReconciler`.  Ideally
	// unique, but not required to be so.
	//
	// +optional
	Name string

	// ChildType is the resource being created/updated/deleted by the reconciler. For example, a
	// reconciled resource Deployment would have a ReplicaSet as a child. Required when the
	// generic type is not a struct, or is unstructured.
	//
	// +optional
	ChildType ChildType
	// ChildListType is the listing type for the child type. For example,
	// PodList is the list type for Pod. Required when the generic type is not
	// a struct, or is unstructured.
	//
	// +optional
	ChildListType ChildListType

	// Finalizer is set on the reconciled resource before a child resource is created, and cleared
	// after a child resource is deleted. The value must be unique to this specific reconciler
	// instance and not shared. Reusing a value may result in orphaned resources when the
	// reconciled resource is deleted.
	//
	// Using a finalizer is encouraged when the Kubernetes garbage collector is unable to delete
	// the child resource automatically, like when the reconciled resource and child are in different
	// namespaces, scopes or clusters.
	//
	// Use of a finalizer implies that SkipOwnerReference is true.
	//
	// +optional
	Finalizer string

	// SkipOwnerReference when true will not create and find child resources via an owner
	// reference. OurChild must be defined for the reconciler to distinguish the child being
	// reconciled from other resources of the same type.
	//
	// Any child resource created is tracked for changes.
	SkipOwnerReference bool

	// Setup performs initialization on the manager and builder this reconciler
	// will run with. It's common to setup field indexes and watch resources.
	//
	// +optional
	Setup func(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error

	// DesiredChildren returns the set of desired child object for the given reconciled resource,
	// or nil if no children should exist. Each resource returned from this method must be claimed
	// by the OurChild method with a stable, unique identifier returned. The identifier is used to
	// correlate desired and actual child resources.
	//
	// Current known children can be obtained via RetrieveKnownChildren[ChildType](ctx). This can
	// be used to keep existing children while stamping out new resources, or for garbage
	// collecting resources based on some criteria. Return the children that should be kept and
	// omit children to delete.
	//
	// To skip reconciliation of the child resources while still reflecting an existing child's
	// status on the reconciled resource, return OnlyReconcileChildStatus as an error.
	DesiredChildren func(ctx context.Context, resource Type) ([]ChildType, error)

	// ChildObjectManager synchronizes the desired child state to the API Server.
	ChildObjectManager ObjectManager[ChildType]

	// ReflectChildrenStatusOnParent updates the reconciled resource's status with values from the
	// child reconciliations. Most errors are returned directly, skipping this method. The set of
	// handled error reasons is defined by ReflectedChildErrorReasons.
	//
	// The default set of reflected errors may change. Implementations should be defensive in
	// handling an unknown error reason.
	//
	// Results contain the union of desired and actual child resources, in the order they were
	// reconciled, (sorted by identifier).
	ReflectChildrenStatusOnParent func(ctx context.Context, parent Type, result ChildSetResult[ChildType])

	// ReflectChildrenStatusOnParentWithError is equivalent to ReflectChildrenStatusOnParent, but
	// also able to return an error.
	ReflectChildrenStatusOnParentWithError func(ctx context.Context, parent Type, result ChildSetResult[ChildType]) error

	// ReflectedChildErrorReasons are client errors when managing the child resource that are
	// handled by ReflectChildrenStatusOnParent. Error reasons not listed are returned directly
	// from the ChildSetReconciler as an error so that the reconcile request can be retried.
	//
	// If not specified, the default reasons are:
	//   - metav1.StatusReasonAlreadyExists
	//   - metav1.StatusReasonForbidden
	//   - metav1.StatusReasonInvalid
	ReflectedChildErrorReasons []metav1.StatusReason

	// ListOptions allows custom options to be use when listing potential child resources. Each
	// resource retrieved as part of the listing is confirmed via OurChild. There is a performance
	// benefit to limiting the number of resource return for each List operation, however,
	// excluding an actual child will orphan that resource.
	//
	// Defaults to filtering by the reconciled resource's namespace:
	//     []client.ListOption{
	//         client.InNamespace(resource.GetNamespace()),
	//     }
	//
	// ListOptions is required when a Finalizer is defined or SkipOwnerReference is true. An empty
	// list is often sufficient although it may incur a performance penalty, especially when
	// querying the API sever instead of an informer cache.
	//
	// +optional
	ListOptions func(ctx context.Context, resource Type) []client.ListOption

	// OurChild is used when there are multiple sources of children of the same ChildType
	// controlled by the same reconciled resource. The function return true for child resources
	// managed by this ChildReconciler. Objects returned from the DesiredChildren function should
	// match this function, otherwise they may be orphaned. If not specified, all children match.
	// Matched child resources must also be uniquely identifiable with the IdentifyChild method.
	//
	// OurChild is required when a Finalizer is defined or SkipOwnerReference is true.
	//
	// +optional
	OurChild func(resource Type, child ChildType) bool

	// IdentifyChild returns a stable identifier for the child resource. The identifier is used to
	// correlate desired child resources with actual child resources. The same value must be returned
	// for an object both before and after it is created on the API server.
	//
	// Non-deterministic IDs will result in the rapid deletion and creation of child resources.
	IdentifyChild func(child ChildType) string

	lazyInit       sync.Once
	voidReconciler *ChildReconciler[Type, ChildType, ChildListType]
}

func (r *ChildSetReconciler[T, CT, CLT]) init() {
	r.lazyInit.Do(func() {
		var nilCT CT
		if internal.IsNil(r.ChildType) {
			r.ChildType = newEmpty(nilCT).(CT)
		}
		if internal.IsNil(r.ChildListType) {
			var nilCLT CLT
			r.ChildListType = newEmpty(nilCLT).(CLT)
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("%sChildSetReconciler", typeName(r.ChildType))
		}
		r.voidReconciler = r.childReconcilerFor(nilCT, nil, "", true)
		if r.ReflectChildrenStatusOnParentWithError == nil && r.ReflectChildrenStatusOnParent != nil {
			r.ReflectChildrenStatusOnParentWithError = func(ctx context.Context, parent T, result ChildSetResult[CT]) error {
				r.ReflectChildrenStatusOnParent(ctx, parent, result)
				return nil
			}
		}
	})
}

func (r *ChildSetReconciler[T, CT, CLT]) SetupWithManager(ctx context.Context, mgr ctrl.Manager, bldr *builder.Builder) error {
	r.init()

	c := RetrieveConfigOrDie(ctx)

	log := logr.FromContextOrDiscard(ctx).
		WithName(r.Name).
		WithValues("childType", gvk(c, r.ChildType))
	ctx = logr.NewContext(ctx, log)

	if err := r.Validate(ctx); err != nil {
		return err
	}

	if err := r.ChildObjectManager.SetupWithManager(ctx, mgr, bldr); err != nil {
		return err
	}

	if err := r.voidReconciler.SetupWithManager(ctx, mgr, bldr); err != nil {
		return err
	}

	if r.Setup != nil {
		if err := r.Setup(ctx, mgr, bldr); err != nil {
			return err
		}
	}

	return nil
}

func (r *ChildSetReconciler[T, CT, CLT]) childReconcilerFor(desired CT, desiredErr error, id string, void bool) *ChildReconciler[T, CT, CLT] {
	return &ChildReconciler[T, CT, CLT]{
		Name:               id,
		ChildType:          r.ChildType,
		ChildListType:      r.ChildListType,
		SkipOwnerReference: r.SkipOwnerReference,
		DesiredChild: func(ctx context.Context, resource T) (CT, error) {
			return desired, desiredErr
		},
		ChildObjectManager: r.ChildObjectManager,
		ReflectChildStatusOnParent: func(ctx context.Context, parent T, child CT, err error) {
			result := childSetResultStasher[CT]().RetrieveOrEmpty(ctx)
			result.Children = append(result.Children, ChildSetPartialResult[CT]{
				Id:    id,
				Child: child,
				Err:   err,
			})
			childSetResultStasher[CT]().Store(ctx, result)
		},
		ReflectedChildErrorReasons: r.ReflectedChildErrorReasons,
		ListOptions:                r.ListOptions,
		OurChild: func(resource T, child CT) bool {
			if r.OurChild != nil && !r.OurChild(resource, child) {
				return false
			}
			return void || id == r.IdentifyChild(child)
		},
	}
}

func (r *ChildSetReconciler[T, CT, CLT]) Validate(ctx context.Context) error {
	r.init()

	// default implicit values
	if r.Finalizer != "" {
		r.SkipOwnerReference = true
	}

	// require DesiredChildren
	if r.DesiredChildren == nil {
		return fmt.Errorf("ChildSetReconciler %q must implement DesiredChildren", r.Name)
	}

	// require ReflectChildrenStatusOnParent or ReflectChildrenStatusOnParentWithError
	if r.ReflectChildrenStatusOnParent == nil && r.ReflectChildrenStatusOnParentWithError == nil {
		return fmt.Errorf("ChildSetReconciler %q must implement ReflectChildrenStatusOnParent or ReflectChildrenStatusOnParentWithError", r.Name)
	}

	if r.OurChild == nil && r.SkipOwnerReference {
		// OurChild is required when SkipOwnerReference is true
		return fmt.Errorf("ChildSetReconciler %q must implement OurChild since owner references are not used", r.Name)
	}

	if r.ListOptions == nil && r.SkipOwnerReference {
		// ListOptions is required when SkipOwnerReference is true
		return fmt.Errorf("ChildSetReconciler %q must implement ListOptions since owner references are not used", r.Name)
	}

	// require IdentifyChild
	if r.IdentifyChild == nil {
		return fmt.Errorf("ChildSetReconciler %q must implement IdentifyChild", r.Name)
	}

	// require ChildObjectManager
	if r.ChildObjectManager == nil {
		return fmt.Errorf("ChildSetReconciler %q must implement ChildObjectManager", r.Name)
	}

	return nil
}

func (r *ChildSetReconciler[T, CT, CLT]) Reconcile(ctx context.Context, resource T) (Result, error) {
	r.init()

	log := logr.FromContextOrDiscard(ctx).
		WithName(r.Name)
	ctx = logr.NewContext(ctx, log)

	knownChildren, err := r.knownChildren(ctx, resource)
	if err != nil {
		return Result{}, err
	}
	ctx = stashKnownChildren(ctx, knownChildren)

	cr, err := r.composeChildReconcilers(ctx, resource, knownChildren)
	if err != nil {
		return Result{}, err
	}
	result, reconcileErr := cr.Reconcile(ctx, resource)
	reflectStatusErr := r.reflectStatus(ctx, resource)
	return result, errors.Join(reconcileErr, reflectStatusErr)
}

func (r *ChildSetReconciler[T, CT, CLT]) knownChildren(ctx context.Context, resource T) ([]CT, error) {
	c := RetrieveConfigOrDie(ctx)

	children := r.ChildListType.DeepCopyObject().(CLT)
	ourChildren := []CT{}
	if err := c.List(ctx, children, r.voidReconciler.listOptions(ctx, resource)...); err != nil {
		return nil, err
	}
	for _, child := range extractItems[CT](children) {
		if !r.voidReconciler.ourChild(resource, child) {
			continue
		}
		ourChildren = append(ourChildren, child.DeepCopyObject().(CT))
	}

	return ourChildren, nil
}

func (r *ChildSetReconciler[T, CT, CLT]) composeChildReconcilers(ctx context.Context, resource T, knownChildren []CT) (SubReconciler[T], error) {
	desiredChildren, desiredChildrenErr := r.DesiredChildren(ctx, resource)
	if desiredChildrenErr != nil && !errors.Is(desiredChildrenErr, OnlyReconcileChildStatus) {
		return nil, desiredChildrenErr
	}

	childIDs := sets.NewString()
	desiredChildByID := map[string]CT{}
	for _, child := range desiredChildren {
		id := r.IdentifyChild(child)
		if id == "" {
			return nil, fmt.Errorf("desired child id may not be empty")
		}
		if childIDs.Has(id) {
			return nil, fmt.Errorf("duplicate child id found: %s", id)
		}
		childIDs.Insert(id)
		desiredChildByID[id] = child
	}

	for _, child := range knownChildren {
		id := r.IdentifyChild(child)
		childIDs.Insert(id)
	}

	sequence := Sequence[T]{}
	for _, id := range childIDs.List() {
		child := desiredChildByID[id]
		cr := r.childReconcilerFor(child, desiredChildrenErr, id, false)
		sequence = append(sequence, cr)
	}

	if r.Finalizer != "" {
		return &WithFinalizer[T]{
			Finalizer:  r.Finalizer,
			Reconciler: sequence,
		}, nil
	}
	return sequence, nil
}

func (r *ChildSetReconciler[T, CT, CLT]) reflectStatus(ctx context.Context, parent T) error {
	result := childSetResultStasher[CT]().Clear(ctx)
	return r.ReflectChildrenStatusOnParentWithError(ctx, parent, result)
}

type ChildSetResult[T client.Object] struct {
	Children []ChildSetPartialResult[T]
}

type ChildSetPartialResult[T client.Object] struct {
	Id    string
	Child T
	Err   error
}

func (r *ChildSetResult[T]) AggregateError() error {
	var errs []error
	for _, childResult := range r.Children {
		errs = append(errs, childResult.Err)
	}
	return utilerrors.NewAggregate(errs)
}

func childSetResultStasher[T client.Object]() Stasher[ChildSetResult[T]] {
	return NewStasher[ChildSetResult[T]]("reconciler.io/runtime:childSetResult")
}

const knownChildrenStashKey StashKey = "reconciler.io/runtime:knownChildren"

// RetrieveKnownChildren returns a copy of the children managed by current ChildSetReconciler. The
// known children can be returned from the DesiredChildren method to preserve existing children, or
// to mutate/delete an existing child.
//
// For example, a child stamper could be implemented by returning existing children from
// DesiredChildren and appending an addition child when a new resource should be created. Likewise
// existing children can be garbage collected by omitting a known child.
func RetrieveKnownChildren[T client.Object](ctx context.Context) []T {
	value := ctx.Value(knownChildrenStashKey)
	if result, ok := value.([]T); ok {
		r := make([]T, len(result))
		for i := range result {
			r[i] = result[i].DeepCopyObject().(T)
		}
		return r
	}
	return nil
}

func stashKnownChildren[T client.Object](ctx context.Context, children []T) context.Context {
	return context.WithValue(ctx, knownChildrenStashKey, children)
}
