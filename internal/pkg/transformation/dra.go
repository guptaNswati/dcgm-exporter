/*
 * Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"context"
	"fmt"
	"log/slog"
	"time"

	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	resourcelistersv1 "k8s.io/client-go/listers/resource/v1"
	resourcelistersv1beta1 "k8s.io/client-go/listers/resource/v1beta1"
	"k8s.io/client-go/tools/cache"

	"github.com/NVIDIA/dcgm-exporter/internal/pkg/kubeclient"
)

const (
	informerResyncPeriod = 10 * time.Minute

	// resourceSliceCacheSyncTimeout bounds how long we wait for the initial
	// ResourceSlice informer cache sync. Discovery may report that v1 (or
	// v1beta1) is served while RBAC denies list/watch on resourceslices; in
	// that case the informer's reflector retries forever and HasSynced never
	// returns true. A bounded wait prevents the exporter from blocking on
	// startup in such misconfigurations.
	resourceSliceCacheSyncTimeout = 30 * time.Second
)

// resourceSliceAPIVersion identifies which ResourceSlice API version the cluster serves.
type resourceSliceAPIVersion int

const (
	resourceSliceAPIUnknown resourceSliceAPIVersion = iota
	resourceSliceAPIV1
	resourceSliceAPIV1beta1
)

func (v resourceSliceAPIVersion) String() string {
	switch v {
	case resourceSliceAPIV1:
		return resourcev1.SchemeGroupVersion.String()
	case resourceSliceAPIV1beta1:
		return resourcev1beta1.SchemeGroupVersion.String()
	default:
		return "unknown"
	}
}

// detectResourceSliceAPIVersion returns the ResourceSlice API version served by
// the cluster. It uses discovery to look for the resourceslices resource under
// resource.k8s.io/v1 first (stable, Kubernetes 1.34+) and falls back to
// resource.k8s.io/v1beta1 (older clusters). Returns resourceSliceAPIUnknown if
// neither GroupVersion is served.
func detectResourceSliceAPIVersion(client kubernetes.Interface) resourceSliceAPIVersion {
	apiVersion := []struct {
		groupVersion string
		version      resourceSliceAPIVersion
	}{
		{resourcev1.SchemeGroupVersion.WithResource("resourceslices").GroupVersion().String(), resourceSliceAPIV1},
		{resourcev1beta1.SchemeGroupVersion.WithResource("resourceslices").GroupVersion().String(), resourceSliceAPIV1beta1},
	}

	for _, a := range apiVersion {
		resources, err := client.Discovery().ServerResourcesForGroupVersion(a.groupVersion)
		if err != nil {
			// Discovery returns an error when the group/version isn't served.
			slog.Debug("ResourceSlice discovery failed", "groupVersion", a.groupVersion, "error", err)
			continue
		}
		for _, r := range resources.APIResources {
			if r.Name == "resourceslices" {
				return a.version
			}
		}
	}
	return resourceSliceAPIUnknown
}

func NewDRAResourceSliceManager() (*DRAResourceSliceManager, error) {
	client, err := kubeclient.GetKubeClient()
	if err != nil {
		return nil, fmt.Errorf("error getting kube client: %w", err)
	}

	apiVersion := detectResourceSliceAPIVersion(client)
	if apiVersion == resourceSliceAPIUnknown {
		return nil, fmt.Errorf("ResourceSlice API not served by cluster (looked for %s and %s)",
			resourcev1.SchemeGroupVersion, resourcev1beta1.SchemeGroupVersion)
	}

	factory := informers.NewSharedInformerFactory(client, informerResyncPeriod)

	m := &DRAResourceSliceManager{factory: factory}

	var hasSynced cache.InformerSynced
	switch apiVersion {
	case resourceSliceAPIV1:
		informer := factory.Resource().V1().ResourceSlices().Informer()
		lister := factory.Resource().V1().ResourceSlices().Lister()
		m.lookup = makeV1Lookup(lister)
		hasSynced = informer.HasSynced
	case resourceSliceAPIV1beta1:
		informer := factory.Resource().V1beta1().ResourceSlices().Informer()
		lister := factory.Resource().V1beta1().ResourceSlices().Lister()
		m.lookup = makeV1beta1Lookup(lister)
		hasSynced = informer.HasSynced
	}

	slog.Info("Using ResourceSlice API", "groupVersion", apiVersion.String())

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelContext = cancel
	factory.Start(ctx.Done())

	// Bound the initial sync. Discovery may say the API is served, but RBAC
	// can still deny list/watch on resourceslices, in which case HasSynced
	// would never become true and startup would hang.
	syncCtx, syncCancel := context.WithTimeout(ctx, resourceSliceCacheSyncTimeout)
	defer syncCancel()
	if !cache.WaitForCacheSync(syncCtx.Done(), hasSynced) {
		cancel()
		if syncCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("ResourceSlice informer cache sync timed out after %s "+
				"(check RBAC: list/watch on resource.k8s.io/resourceslices)", resourceSliceCacheSyncTimeout)
		}
		return nil, fmt.Errorf("ResourceSlice informer cache sync failed")
	}
	return m, nil
}

func (m *DRAResourceSliceManager) Stop() {
	if m.cancelContext != nil {
		m.cancelContext()
	}
	// Ensure factory informers are fully stopped.
	if m.factory != nil {
		m.factory.Shutdown()
	}
}

// GetDeviceInfo returns the mapping UUID and MIG device info if applicable.
// For MIG devices: returns (parentUUID, *DRAMigDeviceInfo).
// For full GPUs:  returns (deviceUUID, nil).
// If the (pool, device) tuple is not known, returns ("", nil).
func (m *DRAResourceSliceManager) GetDeviceInfo(pool, device string) (string, *DRAMigDeviceInfo) {
	if m.lookup == nil {
		return "", nil
	}
	return m.lookup(pool, device)
}

// deviceLookupFunc resolves a (pool, device) tuple to a primary mapping UUID
// and optional MIG info using the version-specific Lister cache. Versions
// differ in their attribute layout (v1 stores attributes directly on the
// device; v1beta1 wraps them under .Basic), so each version supplies its own
// implementation.
type deviceLookupFunc func(pool, device string) (string, *DRAMigDeviceInfo)

func makeV1Lookup(lister resourcelistersv1.ResourceSliceLister) deviceLookupFunc {
	return func(pool, device string) (string, *DRAMigDeviceInfo) {
		slices, err := lister.List(labels.Everything())
		if err != nil {
			slog.Debug("listing v1 ResourceSlices failed", "error", err)
			return "", nil
		}
		for _, s := range slices {
			if s.Spec.Driver != DRAGPUDriverName || s.Spec.Pool.Name != pool {
				continue
			}
			for _, d := range s.Spec.Devices {
				if d.Name != device {
					continue
				}
				if d.Attributes == nil {
					return "", nil
				}
				return buildDeviceMapping(
					getV1AttrString(d.Attributes, "type"),
					getV1AttrString(d.Attributes, "uuid"),
					getV1AttrString(d.Attributes, "parentUUID"),
					getV1AttrString(d.Attributes, "profile"),
				)
			}
		}
		return "", nil
	}
}

func makeV1beta1Lookup(lister resourcelistersv1beta1.ResourceSliceLister) deviceLookupFunc {
	return func(pool, device string) (string, *DRAMigDeviceInfo) {
		slices, err := lister.List(labels.Everything())
		if err != nil {
			slog.Debug("listing v1beta1 ResourceSlices failed", "error", err)
			return "", nil
		}
		for _, s := range slices {
			if s.Spec.Driver != DRAGPUDriverName || s.Spec.Pool.Name != pool {
				continue
			}
			for _, d := range s.Spec.Devices {
				if d.Name != device {
					continue
				}
				if d.Basic == nil || d.Basic.Attributes == nil {
					return "", nil
				}
				return buildDeviceMapping(
					getV1beta1AttrString(d.Basic.Attributes, "type"),
					getV1beta1AttrString(d.Basic.Attributes, "uuid"),
					getV1beta1AttrString(d.Basic.Attributes, "parentUUID"),
					getV1beta1AttrString(d.Basic.Attributes, "profile"),
				)
			}
		}
		return "", nil
	}
}

// buildDeviceMapping converts the four DRA attributes that dcgm-exporter
// cares about into the (uuid, *MIGInfo). Returns
// ("", nil) for an unknown device type or a malformed MIG entry.
func buildDeviceMapping(deviceType, uuid, parentUUID, profile string) (string, *DRAMigDeviceInfo) {
	switch deviceType {
	case "gpu":
		return uuid, nil
	case "mig":
		if parentUUID == "" {
			slog.Debug("MIG device missing parent UUID", "uuid", uuid)
			return "", nil
		}
		return parentUUID, &DRAMigDeviceInfo{
			MIGDeviceUUID: uuid,
			Profile:       profile,
			ParentUUID:    parentUUID,
		}
	default:
		slog.Debug("Unknown DRA device type", "type", deviceType)
		return "", nil
	}
}

func getV1AttrString(attrs map[resourcev1.QualifiedName]resourcev1.DeviceAttribute, key resourcev1.QualifiedName) string {
	if attr, ok := attrs[key]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

func getV1beta1AttrString(attrs map[resourcev1beta1.QualifiedName]resourcev1beta1.DeviceAttribute, key resourcev1beta1.QualifiedName) string {
	if attr, ok := attrs[key]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}
