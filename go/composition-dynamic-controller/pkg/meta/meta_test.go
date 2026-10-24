package meta

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestGetReleaseName(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
	}{
		{
			name:     "release name exists",
			labels:   map[string]string{ReleaseNameLabel: "test-release"},
			expected: "test-release",
		},
		{
			name:     "release name does not exist",
			labels:   map[string]string{"other-label": "value"},
			expected: "",
		},
		{
			name:     "nil labels",
			labels:   nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// obj := &unstructured.U{labels: tt.labels}
			obj := unstructured.Unstructured{}
			obj.SetLabels(tt.labels)
			result := GetReleaseName(&obj)
			if result != tt.expected {
				t.Errorf("GetReleaseName() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestSetReleaseName(t *testing.T) {
	tests := []struct {
		name           string
		initialLabels  map[string]string
		releaseName    string
		expectedLabels map[string]string
	}{
		{
			name:          "set release name on nil labels",
			initialLabels: nil,
			releaseName:   "new-release",
			expectedLabels: map[string]string{
				ReleaseNameLabel: "new-release",
			},
		},
		{
			name:          "set release name on empty labels",
			initialLabels: map[string]string{},
			releaseName:   "new-release",
			expectedLabels: map[string]string{
				ReleaseNameLabel: "new-release",
			},
		},
		{
			name: "set release name with existing labels",
			initialLabels: map[string]string{
				"existing-label": "value",
			},
			releaseName: "new-release",
			expectedLabels: map[string]string{
				"existing-label": "value",
				ReleaseNameLabel: "new-release",
			},
		},
		{
			name: "do not overwrite existing release name",
			initialLabels: map[string]string{
				ReleaseNameLabel: "existing-release",
				"other-label":    "value",
			},
			releaseName: "new-release",
			expectedLabels: map[string]string{
				ReleaseNameLabel: "existing-release",
				"other-label":    "value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := unstructured.Unstructured{}
			obj.SetLabels(tt.initialLabels)
			SetReleaseName(&obj, tt.releaseName)

			labels := obj.GetLabels()
			if len(labels) != len(tt.expectedLabels) {
				t.Errorf("Expected %d labels, got %d", len(tt.expectedLabels), len(labels))
			}

			for key, expectedValue := range tt.expectedLabels {
				if actualValue, exists := labels[key]; !exists || actualValue != expectedValue {
					t.Errorf("Expected label %s=%s, got %s=%s", key, expectedValue, key, actualValue)
				}
			}
		})
	}
}

func TestSetCompositionDefinitionLabels(t *testing.T) {
	tests := []struct {
		name           string
		initialLabels  map[string]string
		cdInfo         CompositionDefinitionInfo
		expectedLabels map[string]string
	}{
		{
			name:          "set labels on nil labels",
			initialLabels: nil,
			cdInfo: CompositionDefinitionInfo{
				Namespace: "test-namespace",
				Name:      "test-name",
				GVR: schema.GroupVersionResource{
					Group:    "test.io",
					Version:  "v1",
					Resource: "testresources",
				},
			},
			expectedLabels: map[string]string{
				CompositionDefinitionNameLabel:      "test-name",
				CompositionDefinitionNamespaceLabel: "test-namespace",
				CompositionDefinitionGroupLabel:     "test.io",
				CompositionDefinitionVersionLabel:   "v1",
				CompositionDefinitionResourceLabel:  "testresources",
			},
		},
		{
			name: "set labels with existing labels",
			initialLabels: map[string]string{
				"existing-label": "value",
			},
			cdInfo: CompositionDefinitionInfo{
				Namespace: "test-namespace",
				Name:      "test-name",
				GVR: schema.GroupVersionResource{
					Group:    "test.io",
					Version:  "v1",
					Resource: "testresources",
				},
			},
			expectedLabels: map[string]string{
				"existing-label":                    "value",
				CompositionDefinitionNameLabel:      "test-name",
				CompositionDefinitionNamespaceLabel: "test-namespace",
				CompositionDefinitionGroupLabel:     "test.io",
				CompositionDefinitionVersionLabel:   "v1",
				CompositionDefinitionResourceLabel:  "testresources",
			},
		},
		{
			name: "overwrite existing composition definition labels",
			initialLabels: map[string]string{
				CompositionDefinitionNameLabel: "old-name",
				"other-label":                  "value",
			},
			cdInfo: CompositionDefinitionInfo{
				Namespace: "new-namespace",
				Name:      "new-name",
				GVR: schema.GroupVersionResource{
					Group:    "new.io",
					Version:  "v2",
					Resource: "newresources",
				},
			},
			expectedLabels: map[string]string{
				"other-label":                       "value",
				CompositionDefinitionNameLabel:      "new-name",
				CompositionDefinitionNamespaceLabel: "new-namespace",
				CompositionDefinitionGroupLabel:     "new.io",
				CompositionDefinitionVersionLabel:   "v2",
				CompositionDefinitionResourceLabel:  "newresources",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := unstructured.Unstructured{}
			obj.SetLabels(tt.initialLabels)
			SetCompositionDefinitionLabels(&obj, tt.cdInfo)

			labels := obj.GetLabels()
			if len(labels) != len(tt.expectedLabels) {
				t.Errorf("Expected %d labels, got %d", len(tt.expectedLabels), len(labels))
			}

			for key, expectedValue := range tt.expectedLabels {
				if actualValue, exists := labels[key]; !exists || actualValue != expectedValue {
					t.Errorf("Expected label %s=%s, got %s=%s", key, expectedValue, key, actualValue)
				}
			}
		})
	}
}
func TestIsGracefullyPaused(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{
			name:        "gracefully paused annotation set to true",
			annotations: map[string]string{AnnotationKeyReconciliationGracefullyPaused: "true"},
			expected:    true,
		},
		{
			name:        "gracefully paused annotation set to false",
			annotations: map[string]string{AnnotationKeyReconciliationGracefullyPaused: "false"},
			expected:    false,
		},
		{
			name:        "gracefully paused annotation set to empty string",
			annotations: map[string]string{AnnotationKeyReconciliationGracefullyPaused: ""},
			expected:    false,
		},
		{
			name:        "gracefully paused annotation does not exist",
			annotations: map[string]string{"other-annotation": "value"},
			expected:    false,
		},
		{
			name:        "nil annotations",
			annotations: nil,
			expected:    false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			expected:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := unstructured.Unstructured{}
			obj.SetAnnotations(tt.annotations)
			result := IsGracefullyPaused(&obj)
			if result != tt.expected {
				t.Errorf("IsGracefullyPaused() = %v, want %v", result, tt.expected)
			}
		})
	}
}
func TestSetGracefullyPausedTime(t *testing.T) {
	tests := []struct {
		name                string
		initialAnnotations  map[string]string
		timeToSet           time.Time
		expectedAnnotations map[string]string
	}{
		{
			name:               "set time on nil annotations",
			initialAnnotations: nil,
			timeToSet:          time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
			expectedAnnotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45Z",
			},
		},
		{
			name:               "set time on empty annotations",
			initialAnnotations: map[string]string{},
			timeToSet:          time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
			expectedAnnotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45Z",
			},
		},
		{
			name: "set time with existing annotations",
			initialAnnotations: map[string]string{
				"existing-annotation": "value",
			},
			timeToSet: time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
			expectedAnnotations: map[string]string{
				"existing-annotation":                           "value",
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45Z",
			},
		},
		{
			name: "overwrite existing paused time",
			initialAnnotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-01-01T00:00:00Z",
				"other-annotation": "value",
			},
			timeToSet: time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
			expectedAnnotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45Z",
				"other-annotation": "value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := unstructured.Unstructured{}
			obj.SetAnnotations(tt.initialAnnotations)
			SetGracefullyPausedTime(&obj, tt.timeToSet)

			annotations := obj.GetAnnotations()
			if len(annotations) != len(tt.expectedAnnotations) {
				t.Errorf("Expected %d annotations, got %d", len(tt.expectedAnnotations), len(annotations))
			}

			for key, expectedValue := range tt.expectedAnnotations {
				if actualValue, exists := annotations[key]; !exists || actualValue != expectedValue {
					t.Errorf("Expected annotation %s=%s, got %s=%s", key, expectedValue, key, actualValue)
				}
			}
		})
	}
}

func TestGetGracefullyPausedTime(t *testing.T) {
	tests := []struct {
		name         string
		annotations  map[string]string
		expectedTime time.Time
		expectedOk   bool
	}{
		{
			name: "valid paused time annotation",
			annotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45Z",
			},
			expectedTime: time.Date(2023, 10, 15, 14, 30, 45, 0, time.UTC),
			expectedOk:   true,
		},
		{
			name: "annotation does not exist",
			annotations: map[string]string{
				"other-annotation": "value",
			},
			expectedTime: time.Time{},
			expectedOk:   false,
		},
		{
			name:         "nil annotations",
			annotations:  nil,
			expectedTime: time.Time{},
			expectedOk:   false,
		},
		{
			name:         "empty annotations",
			annotations:  map[string]string{},
			expectedTime: time.Time{},
			expectedOk:   false,
		},
		{
			name: "invalid time format",
			annotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "invalid-time-format",
			},
			expectedTime: time.Time{},
			expectedOk:   false,
		},
		{
			name: "empty time string",
			annotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "",
			},
			expectedTime: time.Time{},
			expectedOk:   false,
		},
		{
			name: "valid RFC3339 time with timezone",
			annotations: map[string]string{
				AnnotationKeyReconciliationGracefullyPausedTime: "2023-10-15T14:30:45+02:00",
			},
			expectedTime: time.Date(2023, 10, 15, 14, 30, 45, 0, time.FixedZone("", 2*60*60)),
			expectedOk:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := unstructured.Unstructured{}
			obj.SetAnnotations(tt.annotations)

			resultTime, resultOk := GetGracefullyPausedTime(&obj)

			if resultOk != tt.expectedOk {
				t.Errorf("GetGracefullyPausedTime() ok = %v, want %v", resultOk, tt.expectedOk)
			}

			if !resultTime.Equal(tt.expectedTime) {
				t.Errorf("GetGracefullyPausedTime() time = %v, want %v", resultTime, tt.expectedTime)
			}
		})
	}
}
func TestCalculateReleaseName(t *testing.T) {
	tests := []struct {
		name            string
		objName         string
		uid             string
		safeReleaseName bool
		expectedPrefix  string
		isDeterministic bool
	}{
		{
			name:            "Deterministic with UID and safeReleaseName",
			objName:         "my-resource",
			uid:             "5a47edd9-c710-4b4b-b5ea-b6cdf9fc1f58",
			safeReleaseName: true,
			expectedPrefix:  "my-resource-",
			isDeterministic: true,
		},
		{
			name:            "Deterministic without safeReleaseName",
			objName:         "simple-name",
			uid:             "5a47edd9-c710-4b4b-b5ea-b6cdf9fc1f58",
			safeReleaseName: false,
			expectedPrefix:  "simple-name",
			isDeterministic: true,
		},
		{
			name:            "Non-deterministic when UID is missing and safeReleaseName is true",
			objName:         "no-uid-resource",
			uid:             "",
			safeReleaseName: true,
			expectedPrefix:  "no-uid-resource-",
			isDeterministic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &unstructured.Unstructured{}
			obj.SetName(tt.objName)
			if tt.uid != "" {
				obj.SetUID(types.UID(tt.uid))
			}

			// First execution
			r1 := CalculateReleaseName(obj, tt.safeReleaseName)
			// Second execution to check determinism
			r2 := CalculateReleaseName(obj, tt.safeReleaseName)

			// 1. Basic validation
			if r1 == "" {
				t.Fatal("CalculateReleaseName returned an empty string")
			}

			// 2. Prefix validation
			if !strings.HasPrefix(r1, tt.expectedPrefix) {
				t.Errorf("Expected prefix %q, got %q", tt.expectedPrefix, r1)
			}

			// 3. Determinism validation
			if tt.isDeterministic {
				if r1 != r2 {
					t.Errorf("Result should be deterministic but changed: %q vs %q", r1, r2)
				}
			} else {
				// For non-deterministic cases, we expect them to be different
				if r1 == r2 {
					t.Errorf("Result should be random but was identical: %q", r1)
				}
			}

			// 4. Special case: safeReleaseName = false should return EXACTLY the name
			if !tt.safeReleaseName && r1 != tt.objName {
				t.Errorf("Expected exact name %q, got %q", tt.objName, r1)
			}
		})
	}
}

// The origin stamp is only useful if it cannot move. Every reconcile after a chart-version bump
// delivers the object at the NEW version, so a SetOriginVersion that overwrote would record the
// very value the stamp exists to avoid trusting (#168).
func TestSetOriginVersion_WriteOnce(t *testing.T) {
	t.Run("stamps when absent", func(t *testing.T) {
		o := &unstructured.Unstructured{}
		if !SetOriginVersion(o, "v1-8-70") {
			t.Fatal("SetOriginVersion reported no change on a bare object")
		}
		if got := GetOriginVersion(o); got != "v1-8-70" {
			t.Errorf("origin = %q, want v1-8-70", got)
		}
	})

	t.Run("never overwrites an existing stamp", func(t *testing.T) {
		o := &unstructured.Unstructured{}
		SetOriginVersion(o, "v1-8-70")
		if SetOriginVersion(o, "v1-8-71") {
			t.Error("SetOriginVersion reported a change when a stamp was already present")
		}
		if got := GetOriginVersion(o); got != "v1-8-70" {
			t.Errorf("origin was overwritten to %q; the stamp must be immutable", got)
		}
	})

	t.Run("an empty version is not stamped", func(t *testing.T) {
		o := &unstructured.Unstructured{}
		if SetOriginVersion(o, "") {
			t.Error("an empty version should not be written")
		}
		if _, ok := o.GetAnnotations()[AnnotationKeyOriginVersion]; ok {
			t.Error("an empty version must not create the annotation — an empty stamp would be " +
				"indistinguishable from an un-stamped object and must fall back, not lock in")
		}
	})

	t.Run("preserves other annotations", func(t *testing.T) {
		o := &unstructured.Unstructured{}
		o.SetAnnotations(map[string]string{"krateo.io/gracefully-paused": "true"})
		SetOriginVersion(o, "v1-8-70")
		if o.GetAnnotations()["krateo.io/gracefully-paused"] != "true" {
			t.Error("SetOriginVersion clobbered an unrelated annotation")
		}
		if GetOriginVersion(o) != "v1-8-70" {
			t.Error("stamp not written alongside existing annotations")
		}
	})

	t.Run("an un-stamped object reads empty, not a zero version", func(t *testing.T) {
		if got := GetOriginVersion(&unstructured.Unstructured{}); got != "" {
			t.Errorf("GetOriginVersion on a bare object = %q, want \"\"", got)
		}
	})
}
