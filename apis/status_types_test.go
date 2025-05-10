package apis

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	rtime "reconciler.io/runtime/time"
)

const (
	ConditionCreated    string = "Created"
	ConditionConfigured string = "Configured"
)

var conditionSet = NewLivingConditionSet(ConditionCreated, ConditionConfigured)

type TestStatus struct {
	Status `json:",inline"`
}

func (s *TestStatus) InitializeConditions(ctx context.Context) {
	conditionSet.ManageWithContext(ctx, s).InitializeConditions()
}

func TestStatusConditionManager(t *testing.T) {
	now := metav1.Date(2025, time.March, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		ctx     context.Context
		run     func(*testing.T, *TestStatus)
		wantErr error
	}{
		{
			name: "unhappy and ready condition unknown when just initialized",
			ctx:  context.Background(),
			run: func(t *testing.T, s *TestStatus) {
				if actual := s.IsHappy(); actual == true {
					t.Errorf("%s: IsHappy() actually = %v, expected %v", t.Name(), actual, false)
				}

				readyCondition := s.GetCondition(ConditionReady)
				if actual := ConditionIsUnknown(readyCondition); actual != true {
					t.Errorf("%s: ConditionIsUnknown() actually = %v, expected %v", t.Name(), actual, true)
				}
			},
		},
		{
			name: "setting Created condition to false",
			ctx:  context.Background(),
			run: func(t *testing.T, s *TestStatus) {
				s.MarkFalse(ConditionCreated, ConditionCreated, "")

				readyCondition := s.GetCondition(ConditionReady)
				if actual := ConditionIsFalse(readyCondition); actual != true {
					t.Errorf("%s: ConditionIsFalse() actually = %v, expected %v", t.Name(), actual, true)
				}

				conditionCreated := s.GetCondition(ConditionCreated)
				if actual := ConditionIsFalse(conditionCreated); actual != true {
					t.Errorf("%s: ConditionIsFalse() actually = %v, expected %v", t.Name(), actual, true)
				}

				conditionConfigured := s.GetCondition(ConditionConfigured)
				if actual := ConditionIsUnknown(conditionConfigured); actual != true {
					t.Errorf("%s: ConditionIsUnknown() actually = %v, expected %v", t.Name(), actual, true)
				}
			},
		},
		{
			name: "setting all conditions sets the ready condition to true and happy",
			ctx:  rtime.StashNow(context.Background(), now.Time),
			run: func(t *testing.T, s *TestStatus) {
				s.MarkTrue(ConditionCreated, ConditionCreated, "")
				s.MarkTrue(ConditionConfigured, ConditionConfigured, "")

				readyCondition := s.GetCondition(ConditionReady)
				if actual := ConditionIsTrue(readyCondition); actual != true {
					t.Errorf("%s: ConditionIsTrue() actually = %v, expected %v", t.Name(), actual, true)
				}

				// failure to update this test will break it when adding a new terminal condition
				if actual := s.IsHappy(); actual != true {
					t.Errorf("%s: IsHappy() actually = %v, expected %v", t.Name(), actual, true)
				}

				// check time as well
				for _, condition := range s.GetConditions() {
					if equal := condition.LastTransitionTime.Equal(&now); !equal {
						t.Errorf("%s: LastTransitionTime.Equal() actually = %v, expected %v", t.Name(), equal, true)
					}
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &TestStatus{}
			s.InitializeConditions(tt.ctx)
			tt.run(t, s)
		})
	}
}
