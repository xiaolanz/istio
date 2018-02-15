// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:generate sh -c "./generate.sh config.go > types.go"

// Package crd provides an implementation of the config store and cache
// using Kubernetes Custom Resources and the informer framework from Kubernetes
package crd

import (
	"fmt"
	"time"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"

	"istio.io/istio/pkg/log"
	// import GKE cluster authentication plugin
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// import OIDC cluster authentication plugin, e.g. for Tectonic
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
)

// IstioObject is a k8s wrapper interface for config objects
type IstioObject interface {
	runtime.Object
	GetSpec() map[string]interface{}
	SetSpec(map[string]interface{})
	GetObjectMeta() meta_v1.ObjectMeta
	SetObjectMeta(meta_v1.ObjectMeta)
}

// IstioObjectList is a k8s wrapper interface for config lists
type IstioObjectList interface {
	runtime.Object
	GetItems() []IstioObject
}

// Client is a basic REST client for CRDs implementing config store
type Client struct {
	configGroupVersions []*model.ConfigGroupVersion

	// Map of APIVersion to restClient.
	clientset map[string]*restClient

	// domainSuffix for the config metadata
	domainSuffix string
}

type restClient struct {
	// restconfig for REST type descriptors
	restconfig *rest.Config

	// dynamic REST client for accessing config CRDs
	dynamic *rest.RESTClient
}

func apiVersion(configGroupVersion *model.ConfigGroupVersion) string {
	return configGroupVersion.Group() + "/" + configGroupVersion.Version()
}

// CreateRESTConfig for cluster API server, pass empty config file for in-cluster
func CreateRESTConfig(kubeconfig string, configGroupVersion *model.ConfigGroupVersion) (config *rest.Config, err error) {
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	if err != nil {
		return nil, err
	}
	gv := groupVersion(configGroupVersion)
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON

	types := runtime.NewScheme()
	schemeBuilder := runtime.NewSchemeBuilder(
		func(scheme *runtime.Scheme) error {
			for _, s := range configGroupVersion.Schemas() {
				kind := knownTypes[s.Type]
				scheme.AddKnownTypes(gv, kind.object, kind.collection)
			}
			meta_v1.AddToGroupVersion(scheme, gv)
			return nil
		})
	err = schemeBuilder.AddToScheme(types)
	config.NegotiatedSerializer = serializer.DirectCodecFactory{CodecFactory: serializer.NewCodecFactory(types)}

	return nil, err
}

// NewClient creates a client to Kubernetes API using a kubeconfig file.
// Use an empty value for `kubeconfig` to use the in-cluster config.
// If the kubeconfig file is empty, defaults to in-cluster config as well.
func NewClient(config string, configGroupVersions []*model.ConfigGroupVersion, domainSuffix string) (*Client, error) {
	cs := make(map[string]*restClient)
	k, err := kube.ResolveConfig(config)
	if err != nil {
		return nil, err
	}

	for _, group := range configGroupVersions {
		for _, typ := range group.Schemas() {
			if _, exists := knownTypes[typ.Type]; !exists {
				return nil, fmt.Errorf("missing known type for %q", typ.Type)
			}
		}

		rc, err := CreateRESTConfig(k, group)
		if err != nil {
			return nil, err
		}

		dynamic, err := rest.RESTClientFor(rc)
		if err != nil {
			return nil, err
		}

		cs[apiVersion(group)] = &restClient{
			rc,
			dynamic,
		}
	}

	out := &Client{
		configGroupVersions: configGroupVersions,
		clientset:           cs,
		domainSuffix:        domainSuffix,
	}

	return out, nil
}

func groupVersion(cgv *model.ConfigGroupVersion) schema.GroupVersion {
	return schema.GroupVersion{
		Group:   cgv.Group(),
		Version: cgv.Version(),
	}
}

// RegisterResources sends a request to create CRDs and waits for them to initialize
func (cl *Client) RegisterResources() error {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		clientset, err := apiextensionsclient.NewForConfig(rc.restconfig)
		if err != nil {
			return err
		}

		for _, schema := range group.Schemas() {
			g := group.Group()
			name := ResourceName(schema.Plural) + "." + g
			crd := &apiextensionsv1beta1.CustomResourceDefinition{
				ObjectMeta: meta_v1.ObjectMeta{
					Name: name,
				},
				Spec: apiextensionsv1beta1.CustomResourceDefinitionSpec{
					Group:   g,
					Version: group.Version(),
					Scope:   apiextensionsv1beta1.NamespaceScoped,
					Names: apiextensionsv1beta1.CustomResourceDefinitionNames{
						Plural: ResourceName(schema.Plural),
						Kind:   KabobCaseToCamelCase(schema.Type),
					},
				},
			}
			log.Infof("registering CRD %q", name)
			_, err = clientset.ApiextensionsV1beta1().CustomResourceDefinitions().Create(crd)
			if err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
		}

		// wait for CRD being established
		errPoll := wait.Poll(500*time.Millisecond, 60*time.Second, func() (bool, error) {
		descriptor:
			for _, schema := range group.Schemas() {
				name := ResourceName(schema.Plural) + "." + schema.Group()
				crd, errGet := clientset.ApiextensionsV1beta1().CustomResourceDefinitions().Get(name, meta_v1.GetOptions{})
				if errGet != nil {
					return false, errGet
				}
				for _, cond := range crd.Status.Conditions {
					switch cond.Type {
					case apiextensionsv1beta1.Established:
						if cond.Status == apiextensionsv1beta1.ConditionTrue {
							log.Infof("established CRD %q", name)
							continue descriptor
						}
					case apiextensionsv1beta1.NamesAccepted:
						if cond.Status == apiextensionsv1beta1.ConditionFalse {
							log.Warnf("name conflict: %v", cond.Reason)
						}
					}
				}
				log.Infof("missing status condition for %q", name)
				return false, nil
			}
			return true, nil
		})

		if errPoll != nil {
			deleteErr := cl.DeregisterResources()
			if deleteErr != nil {
				return multierror.Append(errPoll, deleteErr)
			}
			return errPoll
		}
	}

	return nil
}

// DeregisterResources removes third party resources
func (cl *Client) DeregisterResources() error {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		clientset, err := apiextensionsclient.NewForConfig(rc.restconfig)
		if err != nil {
			return err
		}

		var errs error
		for _, schema := range group.Schemas() {
			name := ResourceName(schema.Plural) + "." + schema.Group()
			err := clientset.ApiextensionsV1beta1().CustomResourceDefinitions().Delete(name, nil)
			errs = multierror.Append(errs, err)
		}

		return errs
	}
	return nil
}

// ConfigGroupVersions for the store
func (cl *Client) ConfigGroupVersions() []*model.ConfigGroupVersion {
	return cl.configGroupVersions
}

// Get implements store interface
func (cl *Client) Get(typ, name, namespace string) (*model.Config, bool) {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		schema, exists := group.GetByType(typ)
		if !exists {
			continue
		}

		config := knownTypes[typ].object.DeepCopyObject().(IstioObject)
		err := rc.dynamic.Get().
			Namespace(namespace).
			Resource(ResourceName(schema.Plural)).
			Name(name).
			Do().Into(config)

		if err != nil {
			log.Warna(err)
			return nil, false
		}

		out, err := ConvertObject(schema, config, cl.domainSuffix)
		if err != nil {
			log.Warna(err)
			return nil, false
		}
		return out, true
	}
	return nil, false
}

// Create implements store interface
func (cl *Client) Create(config model.Config) (string, error) {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		schema, exists := group.GetByType(config.Type)
		if !exists {
			continue
		}

		if err := schema.Validate(config.Spec); err != nil {
			return "", multierror.Prefix(err, "validation error:")
		}

		out, err := ConvertConfig(schema, config)
		if err != nil {
			return "", err
		}

		obj := knownTypes[schema.Type].object.DeepCopyObject().(IstioObject)
		err = rc.dynamic.Post().
			Namespace(out.GetObjectMeta().Namespace).
			Resource(ResourceName(schema.Plural)).
			Body(out).
			Do().Into(obj)
		if err != nil {
			return "", err
		}

		return obj.GetObjectMeta().ResourceVersion, nil
	}

	return "", fmt.Errorf("unrecognized type %q", config.Type)
}

// Update implements store interface
func (cl *Client) Update(config model.Config) (string, error) {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		schema, exists := group.GetByType(config.Type)
		if !exists {
			continue
		}

		if err := schema.Validate(config.Spec); err != nil {
			return "", multierror.Prefix(err, "validation error:")
		}

		if config.ResourceVersion == "" {
			return "", fmt.Errorf("revision is required")
		}

		out, err := ConvertConfig(schema, config)
		if err != nil {
			return "", err
		}

		obj := knownTypes[schema.Type].object.DeepCopyObject().(IstioObject)
		err = rc.dynamic.Put().
			Namespace(out.GetObjectMeta().Namespace).
			Resource(ResourceName(schema.Plural)).
			Name(out.GetObjectMeta().Name).
			Body(out).
			Do().Into(obj)
		if err != nil {
			return "", err
		}

		return obj.GetObjectMeta().ResourceVersion, nil
	}

	return "", fmt.Errorf("unrecognized type %q", config.Type)
}

// Delete implements store interface
func (cl *Client) Delete(typ, name, namespace string) error {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		schema, exists := group.GetByType(typ)
		if !exists {
			continue
		}

		return rc.dynamic.Delete().
			Namespace(namespace).
			Resource(ResourceName(schema.Plural)).
			Name(name).
			Do().Error()
	}
	return fmt.Errorf("missing type %q", typ)
}

// List implements store interface
func (cl *Client) List(typ, namespace string) ([]model.Config, error) {
	for _, group := range cl.configGroupVersions {
		rc := cl.clientset[apiVersion(group)]
		schema, exists := group.GetByType(typ)
		if !exists {
		}

		list := knownTypes[schema.Type].collection.DeepCopyObject().(IstioObjectList)
		errs := rc.dynamic.Get().
			Namespace(namespace).
			Resource(ResourceName(schema.Plural)).
			Do().Into(list)

		out := make([]model.Config, 0)
		for _, item := range list.GetItems() {
			obj, err := ConvertObject(schema, item, cl.domainSuffix)
			if err != nil {
				errs = multierror.Append(errs, err)
			} else {
				out = append(out, *obj)
			}
		}
		return out, errs
	}
	return nil, fmt.Errorf("missing type %q", typ)
}
