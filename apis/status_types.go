/*
Copyright 2019-2020 the original author or authors.

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

package apis

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// Status is the minimally expected status subresource. Use this or provide your own. It also shows how Conditions are
// expected to be embedded in the Status field.
//
// Example:
//
//	type MyResourceStatus struct {
//		apis.Status `json:",inline"`
//		UsefulMessage string `json:"usefulMessage,omitempty"`
//	}
//
// WARNING: Adding fields to this struct will add them to all resources.
// +k8s:deepcopy-gen=true
type Status struct {
	// ObservedGeneration is the 'Generation' of the resource that
	// was last processed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions the latest available observations of a resource's current state.
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`

	conditionManagerWrapper `json:"-"` // for an explanation why this exists, see below
}

// conditionManagerWrapper exists so as to allow the Status struct to implement the ConditionManager interface.
// The ConditionManager interface cannot be embedded directly because the `deepcopy-gen` generator then fails.
// Embedding an unexported wrapper struct and marking the field as excluded via the `json:"-"` struct tag allows
// the code generation to succeed.
// This wouldn't be necessary if fields could be excluded from `deepcopy-gen`.
// +k8s:deepcopy-gen=false
type conditionManagerWrapper struct {
	ConditionManager
}

// DeepCopyInto is defined as a no-op. It's necessary because `deepcopy-gen` isn't running for unexported structs
// but will still generate a call to it in the parent struct.
func (w *conditionManagerWrapper) DeepCopyInto(_ *conditionManagerWrapper) {}

var _ ConditionsAccessor = (*Status)(nil)

var _ ConditionManager = (*Status)(nil)

var _ ConditionManagerSetter = (*Status)(nil)

// GetConditions implements ConditionsAccessor
func (s *Status) GetConditions() []metav1.Condition {
	return s.Conditions
}

// SetConditions implements ConditionsAccessor
func (s *Status) SetConditions(c []metav1.Condition) {
	s.Conditions = c
}

// GetCondition fetches the condition of the specified type.
func (s *Status) GetCondition(t string) *metav1.Condition {
	for _, cond := range s.Conditions {
		if cond.Type == t {
			return &cond
		}
	}
	return nil
}

// SetConditionManager satisfies the ConditionManagerSetter interface.
func (s *Status) SetConditionManager(cm ConditionManager) {
	s.conditionManagerWrapper = conditionManagerWrapper{ConditionManager: cm}
}
