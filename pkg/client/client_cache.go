/*
Copyright 2018 The Kubernetes Authors.

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

package client

import (
	"fmt"
	"reflect"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// clientCache creates and caches rest clients and metadata for Kubernetes types
type clientCache struct {
	// config is the rest.Config to talk to an apiserver
	config *rest.Config

	// scheme maps go structs to GroupVersionKinds
	scheme *runtime.Scheme

	// mapper maps GroupVersionKinds to Resources
	mapper meta.RESTMapper

	// codecs are used to create a REST client for a gvk
	codecs serializer.CodecFactory

	discoveryClient *discovery.DiscoveryClient

	// resourceByType caches type metadata
	resourceByType map[reflect.Type]*resourceMeta
	mu             sync.RWMutex
}

// newResource maps obj to a Kubernetes Resource and constructs a client for that Resource.
// If the object is a list, the resource represents the item's type instead.
func (c *clientCache) newResource(obj runtime.Object) (*resourceMeta, error) {
	gvk, err := apiutil.GVKForObject(obj, c.scheme)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(gvk.Kind, "List") && meta.IsListType(obj) {
		// if this was a list, treat it as a request for the item's resource
		gvk.Kind = gvk.Kind[:len(gvk.Kind)-4]
	}

	client, err := apiutil.RESTClientForGVK(gvk, c.config, c.codecs)
	if err != nil {
		return nil, err
	}
	mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	return &resourceMeta{Interface: client, mapping: mapping, gvk: gvk}, nil
}

// getResource returns the resource meta information for the given type of object.
// If the object is a list, the resource represents the item's type instead.
func (c *clientCache) getResource(obj runtime.Object) (*resourceMeta, error) {
	typ := reflect.TypeOf(obj)

	// It's better to do creation work twice than to not let multiple
	// people make requests at once
	c.mu.RLock()
	r, known := c.resourceByType[typ]
	c.mu.RUnlock()

	if known {
		return r, nil
	}

	// Initialize a new Client
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.newResource(obj)
	if err != nil {
		return nil, err
	}
	c.resourceByType[typ] = r
	return r, err
}

// getObjMeta returns objMeta containing both type and object metadata and state
func (c *clientCache) getObjMeta(obj runtime.Object) (*objMeta, error) {
	r, err := c.getResource(obj)
	if err != nil {
		return nil, err
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return nil, err
	}
	return &objMeta{resourceMeta: r, Object: m}, err
}

// resourceMeta caches state for a Kubernetes type.
type resourceMeta struct {
	// client is the rest client used to talk to the apiserver
	rest.Interface
	// gvk is the GroupVersionKind of the resourceMeta
	gvk schema.GroupVersionKind
	// mapping is the rest mapping
	mapping *meta.RESTMapping
}

// isNamespaced returns true if the type is namespaced
func (r *resourceMeta) isNamespaced() bool {
	if r.mapping.Scope.Name() == meta.RESTScopeNameRoot {
		return false
	}
	return true
}

// resource returns the resource name of the type
func (r *resourceMeta) resource() string {
	return r.mapping.Resource.Resource
}

// objMeta stores type and object information about a Kubernetes type
type objMeta struct {
	// resourceMeta contains type information for the object
	*resourceMeta

	// Object contains meta data for the object instance
	metav1.Object
}

func (c *clientCache) scaleForResource(inputRes schema.GroupVersionResource) (scaleVersion schema.GroupVersionKind, err error) {
	groupVerResources, err := c.discoveryClient.ServerResourcesForGroupVersion(inputRes.GroupVersion().String())
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("unable to fetch discovery information for %s: %v", inputRes.String(), err)
	}

	for _, resource := range groupVerResources.APIResources {
		resourceParts := strings.SplitN(resource.Name, "/", 2)
		if len(resourceParts) != 2 || resourceParts[0] != inputRes.Resource || resourceParts[1] != "scale" {
			// skip non-scale resources, or scales for resources that we're not looking for
			continue
		}

		scaleGV := inputRes.GroupVersion()
		if resource.Group != "" && resource.Version != "" {
			scaleGV = schema.GroupVersion{
				Group:   resource.Group,
				Version: resource.Version,
			}
		}

		return scaleGV.WithKind(resource.Kind), nil
	}

	return schema.GroupVersionKind{}, fmt.Errorf("could not find scale subresource for %s in discovery information", inputRes.String())
}
