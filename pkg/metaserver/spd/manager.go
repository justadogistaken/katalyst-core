/*
Copyright 2022 The Katalyst Authors.

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

package spd

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	workloadapis "github.com/kubewharf/katalyst-api/pkg/apis/workload/v1alpha1"
	"github.com/kubewharf/katalyst-core/pkg/util"
)

// PerformanceLevel is an enumeration type, the smaller the
// positive value, the better the performance
type PerformanceLevel int

const (
	PerformanceLevelUnknown PerformanceLevel = -1
	PerformanceLevelPerfect PerformanceLevel = 0
	PerformanceLevelGood    PerformanceLevel = 1
	PerformanceLevelPoor    PerformanceLevel = 2

	MaxPerformanceScore float64 = 100
	MinPerformanceScore float64 = 0
)

type IndicatorTarget map[string]util.IndicatorTarget

type ServiceProfilingManager interface {
	// ServiceBusinessPerformanceLevel returns the service business performance level for the given pod
	ServiceBusinessPerformanceLevel(ctx context.Context, pod *v1.Pod) (PerformanceLevel, error)

	// ServiceBusinessPerformanceScore returns the service business performance score for the given pod
	// The score is in range [MinPerformanceScore, MaxPerformanceScore]
	ServiceBusinessPerformanceScore(ctx context.Context, pod *v1.Pod) (float64, error)

	// ServiceSystemPerformanceTarget returns the system performance target for the given pod
	ServiceSystemPerformanceTarget(ctx context.Context, pod *v1.Pod) (IndicatorTarget, error)

	// ServiceBaseline returns whether this pod is baseline
	ServiceBaseline(ctx context.Context, pod *v1.Pod) (bool, error)

	// ServiceExtendedIndicator load the extended indicators and return whether the pod is baseline for the extended indicators
	ServiceExtendedIndicator(ctx context.Context, pod *v1.Pod, indicators interface{}) (bool, error)

	// Run starts the service profiling manager
	Run(ctx context.Context)
}

type DummyPodServiceProfile struct {
	PerformanceLevel PerformanceLevel
	Score            float64
}

type DummyServiceProfilingManager struct {
	podProfiles map[types.UID]DummyPodServiceProfile
}

func (d *DummyServiceProfilingManager) ServiceExtendedIndicator(_ context.Context, _ *v1.Pod, _ interface{}) (bool, error) {
	return false, nil
}

func (d *DummyServiceProfilingManager) ServiceBaseline(_ context.Context, _ *v1.Pod) (bool, error) {
	return false, nil
}

func NewDummyServiceProfilingManager(podProfiles map[types.UID]DummyPodServiceProfile) *DummyServiceProfilingManager {
	return &DummyServiceProfilingManager{podProfiles: podProfiles}
}

func (d *DummyServiceProfilingManager) ServiceBusinessPerformanceLevel(_ context.Context, pod *v1.Pod) (PerformanceLevel, error) {
	profile, ok := d.podProfiles[pod.UID]
	if !ok {
		return PerformanceLevelPerfect, nil
	}
	return profile.PerformanceLevel, nil
}

func (d *DummyServiceProfilingManager) ServiceBusinessPerformanceScore(_ context.Context, pod *v1.Pod) (float64, error) {
	profile, ok := d.podProfiles[pod.UID]
	if !ok {
		return 100, nil
	}
	return profile.Score, nil
}

func (d *DummyServiceProfilingManager) ServiceSystemPerformanceTarget(_ context.Context, _ *v1.Pod) (IndicatorTarget, error) {
	return IndicatorTarget{}, nil
}

func (d *DummyServiceProfilingManager) Run(_ context.Context) {}

var _ ServiceProfilingManager = &DummyServiceProfilingManager{}

type serviceProfilingManager struct {
	fetcher SPDFetcher
}

func (m *serviceProfilingManager) ServiceExtendedIndicator(ctx context.Context, pod *v1.Pod, indicators interface{}) (bool, error) {
	spd, err := m.fetcher.GetSPD(ctx, pod)
	if err != nil {
		return false, err
	}

	extendedBaselineSentinel, err := util.GetSPDExtendedBaselineSentinel(spd)
	if err != nil {
		return false, err
	}

	name, o, err := util.GetExtendedIndicator(indicators)
	if err != nil {
		return false, err
	}

	for _, indicator := range spd.Spec.ExtendedIndicator {
		if indicator.Name != name {
			continue
		}

		object := indicator.Indicators.Object
		if object == nil {
			return false, fmt.Errorf("%s inidators object is nil", name)
		}

		t := reflect.TypeOf(indicators)
		if t.Kind() != reflect.Ptr {
			return false, fmt.Errorf("indicators must be pointers to structs")
		}

		v := reflect.ValueOf(object)
		if !v.CanConvert(t) {
			return false, fmt.Errorf("%s indicators object cannot convert to %v", name, t.Name())
		}

		reflect.ValueOf(indicators).Elem().Set(v.Convert(t).Elem())
		return util.IsExtendedBaselinePod(pod, indicator.BaselinePercent, extendedBaselineSentinel, name)
	}

	return false, errors.NewNotFound(schema.GroupResource{Group: workloadapis.GroupName,
		Resource: strings.ToLower(o.GetObjectKind().GroupVersionKind().Kind)}, name)
}

func (m *serviceProfilingManager) ServiceBaseline(ctx context.Context, pod *v1.Pod) (bool, error) {
	spd, err := m.fetcher.GetSPD(ctx, pod)
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	} else if err != nil {
		return false, nil
	}

	baselineSentinel, err := util.GetSPDBaselineSentinel(spd)
	if err != nil {
		return false, err
	}

	isBaseline, err := util.IsBaselinePod(pod, spd.Spec.BaselinePercent, baselineSentinel)
	if err != nil {
		return false, err
	}

	return isBaseline, nil
}

func NewServiceProfilingManager(fetcher SPDFetcher) ServiceProfilingManager {
	return &serviceProfilingManager{
		fetcher: fetcher,
	}
}

func (m *serviceProfilingManager) ServiceBusinessPerformanceScore(_ context.Context, _ *v1.Pod) (float64, error) {
	// todo: implement service business performance score using spd to calculate
	return MaxPerformanceScore, nil
}

// ServiceBusinessPerformanceLevel gets the service business performance level by spd, and use the poorest business indicator
// performance level as the service business performance level.
func (m *serviceProfilingManager) ServiceBusinessPerformanceLevel(ctx context.Context, pod *v1.Pod) (PerformanceLevel, error) {
	spd, err := m.fetcher.GetSPD(ctx, pod)
	if err != nil {
		return PerformanceLevelUnknown, err
	}

	indicatorTarget, err := util.GetServiceBusinessIndicatorTarget(spd)
	if err != nil {
		return PerformanceLevelUnknown, err
	}

	indicatorValue, err := util.GetServiceBusinessIndicatorValue(spd)
	if err != nil {
		return PerformanceLevelUnknown, err
	}

	indicatorLevelMap := make(map[string]PerformanceLevel)
	for indicatorName, target := range indicatorTarget {
		if _, ok := indicatorValue[indicatorName]; !ok {
			indicatorLevelMap[indicatorName] = PerformanceLevelUnknown
			continue
		}

		if target.UpperBound != nil && indicatorValue[indicatorName] > *target.UpperBound {
			indicatorLevelMap[indicatorName] = PerformanceLevelPoor
		} else if target.LowerBound != nil && indicatorValue[indicatorName] < *target.LowerBound {
			indicatorLevelMap[indicatorName] = PerformanceLevelPerfect
		} else {
			indicatorLevelMap[indicatorName] = PerformanceLevelGood
		}
	}

	// calculate the poorest performance level of indicator as the final performance level
	result := PerformanceLevelUnknown
	for indicator, level := range indicatorLevelMap {
		// if indicator level unknown just return error because indicator current value not found
		if level == PerformanceLevelUnknown {
			return PerformanceLevelUnknown, fmt.Errorf("indicator %s current value not found", indicator)
		}

		// choose the higher value of performance level, which is has poorer performance
		if result < level {
			result = level
		}
	}

	return result, nil
}

// ServiceSystemPerformanceTarget gets the service system performance target by spd and return the indicator name
// and its upper and lower bounds
func (m *serviceProfilingManager) ServiceSystemPerformanceTarget(ctx context.Context, pod *v1.Pod) (IndicatorTarget, error) {
	spd, err := m.fetcher.GetSPD(ctx, pod)
	if err != nil {
		return nil, err
	}

	return util.GetServiceSystemIndicatorTarget(spd)
}

func (m *serviceProfilingManager) Run(ctx context.Context) {
	m.fetcher.Run(ctx)
}
