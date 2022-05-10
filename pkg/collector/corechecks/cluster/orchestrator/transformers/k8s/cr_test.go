/*
 * Unless explicitly stated otherwise all files in this repository are licensed
 * under the Apache License Version 2.0.
 * This product includes software developed at Datadog (https://www.datadoghq.com/).
 * Copyright 2016-2022 Datadog, Inc.
 */

package k8s

import (
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestExtractMetadata(t *testing.T) {

	tests := []struct {
		name                  string
		unstructuredToConvert *unstructured.Unstructured
		want                  *metav1.ObjectMeta
	}{
		{
			name: "convert valid versioned unstructured to versioned object should work",
			unstructuredToConvert: &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "Carp",
					"metadata": map[string]interface{}{
						"creationTimestamp": nil,
						"name":              "noxu",
					},
					"spec": map[string]interface{}{
						"hostname": "example.com",
					},
					"status": map[string]interface{}{},
				},
			},
			want: &metav1.ObjectMeta{Name: "noxu"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equalf(t, tt.want, ExtractMetadata(tt.unstructuredToConvert), "ExtractMetadata(%v)", tt.want)
		})
	}
}

func TestGetKeyByField(t *testing.T) {
	key := "spec.setting.lowWaterMark"
	unstructuredToConvert := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Carp",
			"metadata": map[string]interface{}{
				"creationTimestamp": nil,
				"name":              "noxu",
			},
			"spec": map[string]interface{}{
				"setting": map[string]interface{}{
					"lowWaterMark": 10,
				},
			},
			"status": map[string]interface{}{},
		},
	}
	field := GetKeyByField(key, unstructuredToConvert)
	println(field)
}