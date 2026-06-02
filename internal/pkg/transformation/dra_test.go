/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package transformation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	resourcelistersv1 "k8s.io/client-go/listers/resource/v1"
	resourcelistersv1beta1 "k8s.io/client-go/listers/resource/v1beta1"
	"k8s.io/client-go/tools/cache"
)

// resourceSlicesAPIResource is the metadata that ServerResourcesForGroupVersion
// returns when a cluster serves the resourceslices resource. We use it in
// detectResourceSliceAPIVersion tests to simulate which API versions the
// server advertises.
var resourceSlicesAPIResource = metav1.APIResource{
	Name:       "resourceslices",
	Namespaced: false,
	Kind:       "ResourceSlice",
}

func TestDetectResourceSliceAPIVersion(t *testing.T) {
	v1GV := resourcev1.SchemeGroupVersion.String()
	v1beta1GV := resourcev1beta1.SchemeGroupVersion.String()

	tests := []struct {
		name      string
		resources []*metav1.APIResourceList
		want      resourceSliceAPIVersion
	}{
		{
			name: "v1-only",
			resources: []*metav1.APIResourceList{
				{
					GroupVersion: v1GV,
					APIResources: []metav1.APIResource{resourceSlicesAPIResource},
				},
			},
			want: resourceSliceAPIV1,
		},
		{
			name: "v1beta1-only",
			resources: []*metav1.APIResourceList{
				{
					GroupVersion: v1beta1GV,
					APIResources: []metav1.APIResource{resourceSlicesAPIResource},
				},
			},
			want: resourceSliceAPIV1beta1,
		},
		{
			name: "both-served-prefers-v1",
			resources: []*metav1.APIResourceList{
				{
					GroupVersion: v1GV,
					APIResources: []metav1.APIResource{resourceSlicesAPIResource},
				},
				{
					GroupVersion: v1beta1GV,
					APIResources: []metav1.APIResource{resourceSlicesAPIResource},
				},
			},
			want: resourceSliceAPIV1,
		},
		{
			name:      "neither-served",
			resources: nil,
			want:      resourceSliceAPIUnknown,
		},
		{
			// v1 group is advertised but the resourceslices resource itself
			// is not in the list. Detection must skip v1 (inner-loop check)
			// and fall back to v1beta1.
			name: "v1-group-without-resourceslices-falls-back-to-v1beta1",
			resources: []*metav1.APIResourceList{
				{
					GroupVersion: v1GV,
					APIResources: []metav1.APIResource{
						{Name: "deviceclasses", Namespaced: false, Kind: "DeviceClass"},
					},
				},
				{
					GroupVersion: v1beta1GV,
					APIResources: []metav1.APIResource{resourceSlicesAPIResource},
				},
			},
			want: resourceSliceAPIV1beta1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.Resources = tc.resources

			got := detectResourceSliceAPIVersion(client)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildDeviceMapping(t *testing.T) {
	tests := []struct {
		name        string
		deviceType  string
		uuid        string
		parentUUID  string
		profile     string
		wantUUID    string
		wantMIGInfo *DRAMigDeviceInfo
	}{
		{
			name:        "full-gpu",
			deviceType:  "gpu",
			uuid:        "GPU-abcd",
			wantUUID:    "GPU-abcd",
			wantMIGInfo: nil,
		},
		{
			name:       "mig-with-parent",
			deviceType: "mig",
			uuid:       "MIG-1234",
			parentUUID: "GPU-parent",
			profile:    "1g.6gb",
			wantUUID:   "GPU-parent",
			wantMIGInfo: &DRAMigDeviceInfo{
				MIGDeviceUUID: "MIG-1234",
				Profile:       "1g.6gb",
				ParentUUID:    "GPU-parent",
			},
		},
		{
			// A MIG entry without parentUUID is malformed; the function must
			// drop it rather than registering a series under an empty key.
			name:        "mig-missing-parent",
			deviceType:  "mig",
			uuid:        "MIG-orphan",
			wantUUID:    "",
			wantMIGInfo: nil,
		},
		{
			name:        "unknown-type",
			deviceType:  "tpu",
			uuid:        "TPU-xyz",
			wantUUID:    "",
			wantMIGInfo: nil,
		},
		{
			name:        "empty-type",
			deviceType:  "",
			uuid:        "GPU-abcd",
			wantUUID:    "",
			wantMIGInfo: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotUUID, gotMIG := buildDeviceMapping(tc.deviceType, tc.uuid, tc.parentUUID, tc.profile)
			assert.Equal(t, tc.wantUUID, gotUUID)
			assert.Equal(t, tc.wantMIGInfo, gotMIG)
		})
	}
}

func TestMakeV1Lookup(t *testing.T) {
	stringPtr := func(s string) *string { return &s }
	gpuAttrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		"type": {StringValue: stringPtr("gpu")},
		"uuid": {StringValue: stringPtr("GPU-aaaa")},
	}
	migAttrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
		"type":       {StringValue: stringPtr("mig")},
		"uuid":       {StringValue: stringPtr("MIG-bbbb")},
		"parentUUID": {StringValue: stringPtr("GPU-aaaa")},
		"profile":    {StringValue: stringPtr("1g.6gb")},
	}

	gpuSlice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-gpu"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: DRAGPUDriverName,
			Pool:   resourcev1.ResourcePool{Name: "node-a"},
			Devices: []resourcev1.Device{
				{Name: "gpu-0", Attributes: gpuAttrs},
				{Name: "gpu-1-mig-0", Attributes: migAttrs},
				{Name: "gpu-no-attrs"},
			},
		},
	}
	// Slice belongs to a different driver but shares the pool. The lookup
	// must skip it because lookups are scoped to gpu.nvidia.com.
	otherDriverSlice := &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-other-driver"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver: "other.example.com",
			Pool:   resourcev1.ResourcePool{Name: "node-a"},
			Devices: []resourcev1.Device{
				{Name: "non-nvidia-device", Attributes: gpuAttrs},
			},
		},
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(gpuSlice))
	require.NoError(t, indexer.Add(otherDriverSlice))
	lookup := makeV1Lookup(resourcelistersv1.NewResourceSliceLister(indexer))

	tests := []struct {
		name        string
		pool        string
		device      string
		wantUUID    string
		wantMIGInfo *DRAMigDeviceInfo
	}{
		{
			name:     "gpu-device-resolves-to-uuid",
			pool:     "node-a",
			device:   "gpu-0",
			wantUUID: "GPU-aaaa",
		},
		{
			name:     "mig-device-resolves-to-parent-uuid-with-mig-info",
			pool:     "node-a",
			device:   "gpu-1-mig-0",
			wantUUID: "GPU-aaaa",
			wantMIGInfo: &DRAMigDeviceInfo{
				MIGDeviceUUID: "MIG-bbbb",
				Profile:       "1g.6gb",
				ParentUUID:    "GPU-aaaa",
			},
		},
		{
			// Device exists in the indexer but only on a slice owned by a
			// non-NVIDIA driver. If the driver filter regresses, the lookup
			// would return GPU-aaaa instead of "".
			name:     "different-driver-not-matched",
			pool:     "node-a",
			device:   "non-nvidia-device",
			wantUUID: "",
		},
		{
			name:     "unknown-pool",
			pool:     "node-z",
			device:   "gpu-0",
			wantUUID: "",
		},
		{
			name:     "unknown-device",
			pool:     "node-a",
			device:   "gpu-99",
			wantUUID: "",
		},
		{
			name:     "device-without-attributes",
			pool:     "node-a",
			device:   "gpu-no-attrs",
			wantUUID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotUUID, gotMIG := lookup(tc.pool, tc.device)
			assert.Equal(t, tc.wantUUID, gotUUID)
			assert.Equal(t, tc.wantMIGInfo, gotMIG)
		})
	}
}

func TestMakeV1beta1Lookup(t *testing.T) {
	stringPtr := func(s string) *string { return &s }
	gpuAttrs := map[resourcev1beta1.QualifiedName]resourcev1beta1.DeviceAttribute{
		"type": {StringValue: stringPtr("gpu")},
		"uuid": {StringValue: stringPtr("GPU-aaaa")},
	}
	migAttrs := map[resourcev1beta1.QualifiedName]resourcev1beta1.DeviceAttribute{
		"type":       {StringValue: stringPtr("mig")},
		"uuid":       {StringValue: stringPtr("MIG-bbbb")},
		"parentUUID": {StringValue: stringPtr("GPU-aaaa")},
		"profile":    {StringValue: stringPtr("1g.6gb")},
	}

	gpuSlice := &resourcev1beta1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-gpu"},
		Spec: resourcev1beta1.ResourceSliceSpec{
			Driver: DRAGPUDriverName,
			Pool:   resourcev1beta1.ResourcePool{Name: "node-a"},
			Devices: []resourcev1beta1.Device{
				{Name: "gpu-0", Basic: &resourcev1beta1.BasicDevice{Attributes: gpuAttrs}},
				{Name: "gpu-1-mig-0", Basic: &resourcev1beta1.BasicDevice{Attributes: migAttrs}},
				// nil Basic is the v1beta1 analogue of "no attributes": must
				// not panic and must return ("", nil).
				{Name: "gpu-no-basic"},
			},
		},
	}

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	require.NoError(t, indexer.Add(gpuSlice))
	lookup := makeV1beta1Lookup(resourcelistersv1beta1.NewResourceSliceLister(indexer))

	tests := []struct {
		name        string
		pool        string
		device      string
		wantUUID    string
		wantMIGInfo *DRAMigDeviceInfo
	}{
		{
			name:     "gpu-device-resolves-to-uuid",
			pool:     "node-a",
			device:   "gpu-0",
			wantUUID: "GPU-aaaa",
		},
		{
			name:     "mig-device-resolves-to-parent-uuid-with-mig-info",
			pool:     "node-a",
			device:   "gpu-1-mig-0",
			wantUUID: "GPU-aaaa",
			wantMIGInfo: &DRAMigDeviceInfo{
				MIGDeviceUUID: "MIG-bbbb",
				Profile:       "1g.6gb",
				ParentUUID:    "GPU-aaaa",
			},
		},
		{
			name:     "device-without-basic",
			pool:     "node-a",
			device:   "gpu-no-basic",
			wantUUID: "",
		},
		{
			name:     "unknown-device",
			pool:     "node-a",
			device:   "gpu-99",
			wantUUID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotUUID, gotMIG := lookup(tc.pool, tc.device)
			assert.Equal(t, tc.wantUUID, gotUUID)
			assert.Equal(t, tc.wantMIGInfo, gotMIG)
		})
	}
}

func TestDRAResourceSliceManager_GetDeviceInfo_NilLookup(t *testing.T) {
	// in case of partial init failure, must not
	// panic when GetDeviceInfo is called; it should report "not found".
	m := &DRAResourceSliceManager{}
	uuid, mig := m.GetDeviceInfo("any-pool", "any-device")
	assert.Empty(t, uuid)
	assert.Nil(t, mig)
}
