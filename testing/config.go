/*
Copyright 2022 the original author or authors.

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

package testing

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/scheme"
	clientgotesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"reconciler.io/runtime/duck"
	"reconciler.io/runtime/reconcilers"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ExpectConfig encompasses the creation of a config object using given state, captures observed
// behavior of the reconciler and asserts expected behavior against the observed behavior.
//
// This object is driven implicitly by ReconcilerTestCase and SubReconcilerTestCase. A reconciler
// that needs to interact with multiple configs can create and manage additional ExpectConfigs with
// their own expectations. For example, when a WithConfig reconciler is used the SubReconcilers
// under it use a config separate from the config originally used to load the reconciled resource.
type ExpectConfig struct {
	// Name is used when reporting assertion failures to distinguish configs
	Name string

	// Scheme allows the client to map Go structs to Kubernetes GVKs. All structured resources
	// that are expected to interact with this config should be registered within the scheme.
	Scheme *runtime.Scheme
	// StatusSubResourceTypes is a set of object types that support the status sub-resource. For
	// these types, the only way to modify the resource's status is update or patch the status
	// sub-resource. Patching or updating the main resource will not mutated the status field.
	// Built-in Kubernetes types (e.g. Pod, Deployment, etc) are already accounted for and do not
	// need to be listed.
	//
	// Interacting with a status sub-resource for a type not enumerated as having a status
	// sub-resource will return a not found error.
	StatusSubResourceTypes []client.Object
	// Differ methods to use to compare expected and actual values
	Differ Differ

	// GivenObjects build the kubernetes objects which are present at the onset of reconciliation
	GivenObjects []client.Object
	// APIGivenObjects contains objects that are only available via an API reader instead of the normal cache
	APIGivenObjects []client.Object
	// WithClientBuilder allows a test to modify the fake client initialization.
	WithClientBuilder func(*fake.ClientBuilder) *fake.ClientBuilder
	// WithReactors installs each ReactionFunc into each fake clientset. ReactionFuncs intercept
	// each call to the clientset providing the ability to mutate the resource or inject an error.
	WithReactors []ReactionFunc
	// GivenAPIResources populates the fake discovery client and RESTMapper
	GivenAPIResources []*metav1.APIResourceList
	// GivenTracks provide a set of tracked resources to seed the tracker with
	GivenTracks []TrackRequest

	// side effects

	// ExpectTracks holds the ordered list of Track calls expected during reconciliation
	ExpectTracks []TrackRequest
	// ExpectEvents holds the ordered list of events recorded during the reconciliation
	ExpectEvents []Event
	// ExpectCreates builds the ordered list of objects expected to be created during reconciliation
	ExpectCreates []client.Object
	// ExpectUpdates builds the ordered list of objects expected to be updated during reconciliation
	ExpectUpdates []client.Object
	// ExpectPatches builds the ordered list of objects expected to be patched during reconciliation
	ExpectPatches []PatchRef
	// ExpectDeletes holds the ordered list of objects expected to be deleted during reconciliation
	ExpectDeletes []DeleteRef
	// ExpectDeleteCollections holds the ordered list of collections expected to be deleted during reconciliation
	ExpectDeleteCollections []DeleteCollectionRef
	// ExpectStatusUpdates builds the ordered list of objects whose status is updated during reconciliation
	ExpectStatusUpdates []client.Object
	// ExpectStatusPatches builds the ordered list of objects whose status is patched during reconciliation
	ExpectStatusPatches []PatchRef

	once           sync.Once
	client         *clientWrapper
	apiReader      *clientWrapper
	discovery      *fakediscovery.FakeDiscovery
	recorder       *eventRecorder
	tracker        *mockTracker
	observedErrors []string
}

func (c *ExpectConfig) init() {
	c.once.Do(func() {
		// copy given objects to unwrap factories and prevent accidental mutations leaking between test cases
		givenObjects := make([]client.Object, len(c.GivenObjects))
		for i := range c.GivenObjects {
			givenObjects[i] = c.GivenObjects[i].DeepCopyObject().(client.Object)
		}
		apiGivenObjects := make([]client.Object, len(c.APIGivenObjects))
		for i := range c.APIGivenObjects {
			apiGivenObjects[i] = c.APIGivenObjects[i].DeepCopyObject().(client.Object)
		}
		restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{})
		for _, resources := range c.GivenAPIResources {
			if resources == nil {
				continue
			}
			for _, resource := range resources.APIResources {
				kind := schema.GroupVersionKind{Group: resource.Group, Version: resource.Version, Kind: resource.Kind}
				singular := kind.GroupVersion().WithResource(resource.SingularName)
				plural := kind.GroupVersion().WithResource(resource.Name)
				scope := meta.RESTScopeRoot
				if resource.Namespaced {
					scope = meta.RESTScopeNamespace
				}
				restMapper.AddSpecific(kind, plural, singular, scope)
			}
		}

		c.client = c.createClient(givenObjects, c.StatusSubResourceTypes, restMapper)
		for i := range c.WithReactors {
			// in reverse order since we prepend
			reactor := c.WithReactors[len(c.WithReactors)-1-i]
			c.client.PrependReactor("*", "*", reactor)
		}
		c.apiReader = c.createClient(apiGivenObjects, c.StatusSubResourceTypes, restMapper)
		c.discovery = &fakediscovery.FakeDiscovery{
			FakedServerVersion: &version.Info{},
			Fake: &clientgotesting.Fake{
				Resources: c.GivenAPIResources,
			},
		}
		c.recorder = &eventRecorder{
			events: []Event{},
			scheme: c.Scheme,
		}
		c.tracker = createTracker(c.GivenTracks, c.Scheme)
		c.observedErrors = []string{}
		if c.Differ == nil {
			c.Differ = DefaultDiffer
		}
	})
}

func (c *ExpectConfig) configNameMsg() string {
	if c.Name == "" || c.Name == "default" {
		return ""
	}
	return fmt.Sprintf(" for config %q", c.Name)
}

func (c *ExpectConfig) createClient(objs []client.Object, statusSubResourceTypes []client.Object, restMapper meta.RESTMapper) *clientWrapper {
	tracker := clientgotesting.NewObjectTracker(c.Scheme, scheme.Codecs.UniversalDecoder())

	builder := fake.NewClientBuilder()
	builder.WithObjectTracker(tracker)
	builder.WithScheme(c.Scheme)
	builder.WithStatusSubresource(c.normalizeDucks(statusSubResourceTypes)...)
	builder.WithObjects(prepareObjects(c.normalizeDucks(objs))...)
	if c.WithClientBuilder != nil {
		builder = c.WithClientBuilder(builder)
	}
	builder.WithRESTMapper(restMapper)

	return NewFakeClientWrapper(duck.NewDuckAwareClientWrapper(builder.Build()), tracker)
}

func (c *ExpectConfig) normalizeDucks(objs []client.Object) []client.Object {
	normalized := []client.Object{}
	for _, obj := range objs {
		if duck.IsDuck(obj, c.Scheme) {
			u := &unstructured.Unstructured{}
			if err := duck.Convert(obj, u); err != nil {
				panic(err)
			}
			normalized = append(normalized, u)
		} else {
			normalized = append(normalized, obj)
		}
	}
	return normalized
}

// Config returns the Config object. This method should only be called once. Subsequent calls are
// ignored returning the Config from the first call.
func (c *ExpectConfig) Config() reconcilers.Config {
	c.init()
	return reconcilers.Config{
		Client:    c.client,
		APIReader: c.apiReader,
		Discovery: c.discovery,
		Recorder:  c.recorder,
		Tracker:   c.tracker,
	}
}

func (c *ExpectConfig) errorf(t *testing.T, message string, args ...interface{}) {
	if t != nil {
		t.Errorf(message, args...)
	}
	c.observedErrors = append(c.observedErrors, fmt.Sprintf(message, args...))
}

// AssertExpectations asserts all observed reconciler behavior matches the expected behavior
func (c *ExpectConfig) AssertExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	c.AssertClientExpectations(t)
	c.AssertRecorderExpectations(t)
	c.AssertTrackerExpectations(t)
}

// AssertClientExpectations asserts observed reconciler client behavior matches the expected client behavior
func (c *ExpectConfig) AssertClientExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	c.AssertClientCreateExpectations(t)
	c.AssertClientUpdateExpectations(t)
	c.AssertClientPatchExpectations(t)
	c.AssertClientDeleteExpectations(t)
	c.AssertClientDeleteCollectionExpectations(t)
	c.AssertClientStatusUpdateExpectations(t)
	c.AssertClientStatusPatchExpectations(t)
}

// AssertClientCreateExpectations asserts observed reconciler client create behavior matches the expected client create behavior
func (c *ExpectConfig) AssertClientCreateExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	c.compareActions(t, "Create", c.ExpectCreates, c.client.CreateActions, c.Differ.ResourceCreate)
}

// AssertClientUpdateExpectations asserts observed reconciler client update behavior matches the expected client update behavior
func (c *ExpectConfig) AssertClientUpdateExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	c.compareActions(t, "Update", c.ExpectUpdates, c.client.UpdateActions, c.Differ.ResourceUpdate)
}

// AssertClientPatchExpectations asserts observed reconciler client patch behavior matches the expected client patch behavior
func (c *ExpectConfig) AssertClientPatchExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	for i, exp := range c.ExpectPatches {
		if i >= len(c.client.PatchActions) {
			c.errorf(t, "ExpectPatches[%d] not observed%s: %#v", i, c.configNameMsg(), exp)
			continue
		}
		actual := NewPatchRef(c.client.PatchActions[i])

		if diff := c.Differ.PatchRef(exp, actual); diff != "" {
			c.errorf(t, "ExpectPatches[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, expected := len(c.client.PatchActions), len(c.ExpectPatches); actual > expected {
		for _, extra := range c.client.PatchActions[expected:] {
			c.errorf(t, "Unexpected Patch observed%s: %#v", c.configNameMsg(), extra)
		}
	}
}

// AssertClientDeleteExpectations asserts observed reconciler client delete behavior matches the expected client delete behavior
func (c *ExpectConfig) AssertClientDeleteExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	for i, exp := range c.ExpectDeletes {
		if i >= len(c.client.DeleteActions) {
			c.errorf(t, "ExpectDeletes[%d] not observed%s: %#v", i, c.configNameMsg(), exp)
			continue
		}
		actual := NewDeleteRef(c.client.DeleteActions[i])

		if diff := c.Differ.DeleteRef(exp, actual); diff != "" {
			c.errorf(t, "ExpectDeletes[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, expected := len(c.client.DeleteActions), len(c.ExpectDeletes); actual > expected {
		for _, extra := range c.client.DeleteActions[expected:] {
			c.errorf(t, "Unexpected Delete observed%s: %#v", c.configNameMsg(), extra)
		}
	}
}

// AssertClientDeleteCollectionExpectations asserts observed reconciler client delete collection behavior matches the expected client delete collection behavior
func (c *ExpectConfig) AssertClientDeleteCollectionExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	for i, exp := range c.ExpectDeleteCollections {
		if i >= len(c.client.DeleteCollectionActions) {
			c.errorf(t, "ExpectDeleteCollections[%d] not observed%s: %#v", i, c.configNameMsg(), exp)
			continue
		}
		actual := NewDeleteCollectionRef(c.client.DeleteCollectionActions[i])

		if diff := c.Differ.DeleteCollectionRef(exp, actual); diff != "" {
			c.errorf(t, "ExpectDeleteCollections[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, expected := len(c.client.DeleteCollectionActions), len(c.ExpectDeleteCollections); actual > expected {
		for _, extra := range c.client.DeleteCollectionActions[expected:] {
			c.errorf(t, "Unexpected DeleteCollection observed%s: %#v", c.configNameMsg(), extra)
		}
	}
}

// AssertClientStatusUpdateExpectations asserts observed reconciler client status update behavior matches the expected client status update behavior
func (c *ExpectConfig) AssertClientStatusUpdateExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	c.compareActions(t, "StatusUpdate", c.ExpectStatusUpdates, c.client.StatusUpdateActions, c.Differ.ResourceStatusUpdate)
}

// AssertClientStatusPatchExpectations asserts observed reconciler client status patch behavior matches the expected client status patch behavior
func (c *ExpectConfig) AssertClientStatusPatchExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	for i, exp := range c.ExpectStatusPatches {
		if i >= len(c.client.StatusPatchActions) {
			c.errorf(t, "ExpectStatusPatches[%d] not observed%s: %#v", i, c.configNameMsg(), exp)
			continue
		}
		actual := NewPatchRef(c.client.StatusPatchActions[i])

		if diff := c.Differ.PatchRef(exp, actual); diff != "" {
			c.errorf(t, "ExpectStatusPatches[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, expected := len(c.client.StatusPatchActions), len(c.ExpectStatusPatches); actual > expected {
		for _, extra := range c.client.StatusPatchActions[expected:] {
			c.errorf(t, "Unexpected StatusPatch observed%s: %#v", c.configNameMsg(), extra)
		}
	}
}

// AssertRecorderExpectations asserts observed event recorder behavior matches the expected event recorder behavior
func (c *ExpectConfig) AssertRecorderExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	actualEvents := c.recorder.events
	for i, exp := range c.ExpectEvents {
		if i >= len(actualEvents) {
			c.errorf(t, "ExpectEvents[%d] not observed%s: %s", i, c.configNameMsg(), exp)
			continue
		}

		if diff := c.Differ.Event(exp, actualEvents[i]); diff != "" {
			c.errorf(t, "ExpectEvents[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, exp := len(actualEvents), len(c.ExpectEvents); actual > exp {
		for _, extra := range actualEvents[exp:] {
			c.errorf(t, "Unexpected Event observed%s: %s", c.configNameMsg(), extra)
		}
	}
}

// AssertTrackerExpectations asserts observed tracker behavior matches the expected tracker behavior
func (c *ExpectConfig) AssertTrackerExpectations(t *testing.T) {
	if t != nil {
		t.Helper()
	}
	c.init()

	actualTracks := c.tracker.getTrackRequests()
	for i, exp := range c.ExpectTracks {
		exp.normalize()

		if i >= len(actualTracks) {
			c.errorf(t, "ExpectTracks[%d] not observed%s: %v", i, c.configNameMsg(), exp)
			continue
		}

		if diff := c.Differ.TrackRequest(exp, actualTracks[i]); diff != "" {
			c.errorf(t, "ExpectTracks[%d] differs%s (%s, %s):\n%s", i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, exp := len(actualTracks), len(c.ExpectTracks); actual > exp {
		for _, extra := range actualTracks[exp:] {
			c.errorf(t, "Unexpected Track observed%s: %v", c.configNameMsg(), extra)
		}
	}
}

func (c *ExpectConfig) compareActions(t *testing.T, actionName string, expectedActionFactories []client.Object, actualActions []objectAction, differ func(client.Object, client.Object) string) {
	if t != nil {
		t.Helper()
	}
	c.init()

	for i, exp := range expectedActionFactories {
		if i >= len(actualActions) {
			c.errorf(t, "Expect%ss[%d] not observed%s: %#v", actionName, i, c.configNameMsg(), exp.DeepCopyObject())
			continue
		}
		actual := actualActions[i].GetObject()

		if diff := differ(exp.DeepCopyObject().(client.Object), actual.(client.Object)); diff != "" {
			c.errorf(t, "Expect%ss[%d] differs%s (%s, %s):\n%s", actionName, i, c.configNameMsg(), DiffRemovedColor.Sprint("-expected"), DiffAddedColor.Sprint("+actual"), ColorizeDiff(diff))
		}
	}
	if actual, expected := len(actualActions), len(expectedActionFactories); actual > expected {
		for _, extra := range actualActions[expected:] {
			c.errorf(t, "Unexpected %s observed%s: %#v", actionName, c.configNameMsg(), extra)
		}
	}
}

var (
	IgnoreLastTransitionTime = cmp.FilterPath(func(p cmp.Path) bool {
		str := p.String()
		gostr := p.GoString()
		return strings.HasSuffix(str, "LastTransitionTime") ||
			strings.HasSuffix(gostr, `["lastTransitionTime"]`)
	}, cmp.Ignore())
	IgnoreTypeMeta = cmp.FilterPath(func(p cmp.Path) bool {
		str := p.String()
		// only ignore for typed resources, compare TypeMeta values for unstructured
		return strings.HasSuffix(str, "TypeMeta.APIVersion") ||
			strings.HasSuffix(str, "TypeMeta.Kind")
	}, cmp.Ignore())
	IgnoreCreationTimestamp = cmp.FilterPath(func(p cmp.Path) bool {
		str := p.String()
		gostr := p.GoString()
		return strings.HasSuffix(str, "ObjectMeta.CreationTimestamp") ||
			strings.HasSuffix(gostr, `(*unstructured.Unstructured).Object["metadata"].(map[string]any)["creationTimestamp"]`) ||
			strings.HasSuffix(gostr, `{*unstructured.Unstructured}.Object["metadata"].(map[string]any)["creationTimestamp"]`) ||
			strings.HasSuffix(gostr, `(*unstructured.Unstructured).Object["metadata"].(map[string]interface {})["creationTimestamp"]`) ||
			strings.HasSuffix(gostr, `{*unstructured.Unstructured}.Object["metadata"].(map[string]interface {})["creationTimestamp"]`)
	}, cmp.Ignore())
	IgnoreResourceVersion = cmp.FilterPath(func(p cmp.Path) bool {
		str := p.String()
		gostr := p.GoString()
		return strings.HasSuffix(str, "ObjectMeta.ResourceVersion") ||
			strings.HasSuffix(gostr, `(*unstructured.Unstructured).Object["metadata"].(map[string]any)["resourceVersion"]`) ||
			strings.HasSuffix(gostr, `{*unstructured.Unstructured}.Object["metadata"].(map[string]any)["resourceVersion"]`) ||
			strings.HasSuffix(gostr, `(*unstructured.Unstructured).Object["metadata"].(map[string]interface {})["resourceVersion"]`) ||
			strings.HasSuffix(gostr, `{*unstructured.Unstructured}.Object["metadata"].(map[string]interface {})["resourceVersion"]`)
	}, cmp.Ignore())

	statusSubresourceOnly = cmp.FilterPath(func(p cmp.Path) bool {
		str := p.String()
		return str != "" && !strings.HasPrefix(str, "Status")
	}, cmp.Ignore())

	NormalizeLabelSelector = cmp.Transformer("labels.Selector", func(s labels.Selector) *string {
		if s == nil || s.Empty() {
			return nil
		}
		return ptr.To[string](s.String())
	})
	NormalizeFieldSelector = cmp.Transformer("fields.Selector", func(s fields.Selector) *string {
		if s == nil || s.Empty() {
			return nil
		}
		return ptr.To[string](s.String())
	})
)

type PatchRef struct {
	Group       string
	Kind        string
	Namespace   string
	Name        string
	SubResource string
	PatchType   types.PatchType
	Patch       []byte
}

func NewPatchRef(action PatchAction) PatchRef {
	return PatchRef{
		Group:       action.GetResource().Group,
		Kind:        action.GetResource().Resource,
		Namespace:   action.GetNamespace(),
		Name:        action.GetName(),
		SubResource: action.GetSubresource(),
		PatchType:   action.GetPatchType(),
		Patch:       action.GetPatch(),
	}
}

type DeleteRef struct {
	Group     string
	Kind      string
	Namespace string
	Name      string
}

func NewDeleteRef(action DeleteAction) DeleteRef {
	return DeleteRef{
		Group:     action.GetResource().Group,
		Kind:      action.GetResource().Resource,
		Namespace: action.GetNamespace(),
		Name:      action.GetName(),
	}
}

func NewDeleteRefFromObject(obj client.Object, scheme *runtime.Scheme) DeleteRef {
	gvks, _, err := scheme.ObjectKinds(obj.DeepCopyObject())
	if err != nil {
		panic(err)
	}

	return DeleteRef{
		Group:     gvks[0].Group,
		Kind:      gvks[0].Kind,
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

type DeleteCollectionRef struct {
	Group     string
	Kind      string
	Namespace string
	Labels    labels.Selector
	Fields    fields.Selector
}

func NewDeleteCollectionRef(action DeleteCollectionAction) DeleteCollectionRef {
	return DeleteCollectionRef{
		Group:     action.GetResource().Group,
		Kind:      action.GetResource().Resource,
		Namespace: action.GetNamespace(),
		Labels:    action.GetListRestrictions().Labels,
		Fields:    action.GetListRestrictions().Fields,
	}
}
