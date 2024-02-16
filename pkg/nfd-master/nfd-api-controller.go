/*
Copyright 2021-2022 The Kubernetes Authors.

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

package nfdmaster

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sclient "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	nfdclientset "sigs.k8s.io/node-feature-discovery/api/generated/clientset/versioned"
	nfdscheme "sigs.k8s.io/node-feature-discovery/api/generated/clientset/versioned/scheme"
	nfdinformers "sigs.k8s.io/node-feature-discovery/api/generated/informers/externalversions"
	nfdlisters "sigs.k8s.io/node-feature-discovery/api/generated/listers/nfd/v1alpha1"
	nfdv1alpha1 "sigs.k8s.io/node-feature-discovery/api/nfd/v1alpha1"
	"sigs.k8s.io/node-feature-discovery/pkg/utils"
)

type nfdController struct {
	featureLister      nfdlisters.NodeFeatureLister
	ruleLister         nfdlisters.NodeFeatureRuleLister
	featureGroupLister nfdlisters.NodeFeatureGroupLister

	stopChan chan struct{}

	updateAllNodesChan             chan struct{}
	updateOneNodeChan              chan string
	updateAllNodeFeatureGroupsChan chan struct{}
	updateNodeFeatureGroupChan     chan string

	namespaceLister *NamespaceLister
}

type nfdApiControllerOptions struct {
	DisableNodeFeature           bool
	DisableNodeFeatureGroup      bool
	ResyncPeriod                 time.Duration
	K8sClient                    k8sclient.Interface
	NodeFeatureNamespaceSelector *metav1.LabelSelector
}

func init() {
	utilruntime.Must(nfdv1alpha1.AddToScheme(nfdscheme.Scheme))
}

func newNfdController(config *restclient.Config, nfdApiControllerOptions nfdApiControllerOptions) (*nfdController, error) {
	c := &nfdController{
		stopChan:                       make(chan struct{}),
		updateAllNodesChan:             make(chan struct{}, 1),
		updateOneNodeChan:              make(chan string),
		updateAllNodeFeatureGroupsChan: make(chan struct{}, 1),
		updateNodeFeatureGroupChan:     make(chan string),
	}

	labelMap, err := metav1.LabelSelectorAsSelector(nfdApiControllerOptions.NodeFeatureNamespaceSelector)
	if err != nil {
		klog.ErrorS(err, "failed to convert label selector to map", "selector", nfdApiControllerOptions.NodeFeatureNamespaceSelector)
		return nil, err
	}

	// TODO(TessaIO): only start namespace lister if namespace selector is different than nil
	c.namespaceLister = NewNamespaceLister(nfdApiControllerOptions.K8sClient, labelMap)

	nfdClient := nfdclientset.NewForConfigOrDie(config)
	klog.V(2).InfoS("initializing new NFD API controller", "options", utils.DelayedDumper(nfdApiControllerOptions))

	informerFactory := nfdinformers.NewSharedInformerFactory(nfdClient, nfdApiControllerOptions.ResyncPeriod)

	// Add informer for NodeFeature objects
	if !nfdApiControllerOptions.DisableNodeFeature {
		featureInformer := informerFactory.Nfd().V1alpha1().NodeFeatures()
		if _, err := featureInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				nfr := obj.(*nfdv1alpha1.NodeFeature)
				klog.V(2).InfoS("NodeFeature added", "nodefeature", klog.KObj(nfr))
				if c.isNamespaceSelected(nfr.Namespace) {
					c.updateOneNode("NodeFeature", nfr)
				} else {
					klog.InfoS("NodeFeature namespace is not selected, skipping", "nodefeature", klog.KObj(nfr))
				}
				if !nfdApiControllerOptions.DisableNodeFeatureGroup {
					c.updateAllNodeFeatureGroups()
				}
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				nfr := newObj.(*nfdv1alpha1.NodeFeature)
				klog.V(2).InfoS("NodeFeature updated", "nodefeature", klog.KObj(nfr))
				if c.isNamespaceSelected(nfr.Namespace) {
					c.updateOneNode("NodeFeature", nfr)
				} else {
					klog.InfoS("NodeFeature namespace is not selected, skipping", "nodefeature", klog.KObj(nfr))
				}
				if !nfdApiControllerOptions.DisableNodeFeatureGroup {
					c.updateAllNodeFeatureGroups()
				}
			},
			DeleteFunc: func(obj interface{}) {
				nfr := obj.(*nfdv1alpha1.NodeFeature)
				klog.V(2).InfoS("NodeFeature deleted", "nodefeature", klog.KObj(nfr))
				if c.isNamespaceSelected(nfr.Namespace) {
					c.updateOneNode("NodeFeature", nfr)
				} else {
					klog.InfoS("NodeFeature namespace is not selected, skipping", "nodefeature", klog.KObj(nfr))
				}
				if !nfdApiControllerOptions.DisableNodeFeatureGroup {
					c.updateAllNodeFeatureGroups()
				}
			},
		}); err != nil {
			return nil, err
		}
		c.featureLister = featureInformer.Lister()
	}

	// Add informer for NodeFeatureRule objects
	nodeFeatureRuleInformer := informerFactory.Nfd().V1alpha1().NodeFeatureRules()
	if _, err := nodeFeatureRuleInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(object interface{}) {
			klog.V(2).InfoS("NodeFeatureRule added", "nodefeaturerule", klog.KObj(object.(metav1.Object)))
			if !nfdApiControllerOptions.DisableNodeFeature {
				c.updateAllNodes()
			}
			// else: rules will be processed only when gRPC requests are received
		},
		UpdateFunc: func(oldObject, newObject interface{}) {
			klog.V(2).InfoS("NodeFeatureRule updated", "nodefeaturerule", klog.KObj(newObject.(metav1.Object)))
			if !nfdApiControllerOptions.DisableNodeFeature {
				c.updateAllNodes()
			}
			// else: rules will be processed only when gRPC requests are received
		},
		DeleteFunc: func(object interface{}) {
			klog.V(2).InfoS("NodeFeatureRule deleted", "nodefeaturerule", klog.KObj(object.(metav1.Object)))
			if !nfdApiControllerOptions.DisableNodeFeature {
				c.updateAllNodes()
			}
			// else: rules will be processed only when gRPC requests are received
		},
	}); err != nil {
		return nil, err
	}
	c.ruleLister = nodeFeatureRuleInformer.Lister()

	// Add informer for NodeFeatureGroup objects
	if !nfdApiControllerOptions.DisableNodeFeatureGroup {
		nodeFeatureGroupInformer := informerFactory.Nfd().V1alpha1().NodeFeatureGroups()
		if _, err := nodeFeatureGroupInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				nfg := obj.(*nfdv1alpha1.NodeFeatureGroup)
				klog.V(2).InfoS("NodeFeatureGroup added", "nodeFeatureGroup", klog.KObj(nfg))
				c.updateNodeFeatureGroup(nfg.Name)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				nfg := newObj.(*nfdv1alpha1.NodeFeatureGroup)
				klog.V(2).InfoS("NodeFeatureGroup updated", "nodeFeatureGroup", klog.KObj(nfg))
				c.updateNodeFeatureGroup(nfg.Name)
			},
			DeleteFunc: func(obj interface{}) {
				nfg := obj.(*nfdv1alpha1.NodeFeatureGroup)
				klog.V(2).InfoS("NodeFeatureGroup deleted", "nodeFeatureGroup", klog.KObj(nfg))
				c.updateNodeFeatureGroup(nfg.Name)
			},
		}); err != nil {
			return nil, err
		}
		c.featureGroupLister = nodeFeatureGroupInformer.Lister()
	}

	// Start informers
	informerFactory.Start(c.stopChan)
	informerFactory.WaitForCacheSync(c.stopChan)

	return c, nil
}

func (c *nfdController) stop() {
	close(c.stopChan)
	c.namespaceLister.stop()
}

func getNodeNameForObj(obj metav1.Object) (string, error) {
	nodeName, ok := obj.GetLabels()[nfdv1alpha1.NodeFeatureObjNodeNameLabel]
	if !ok {
		return "", fmt.Errorf("%q label is missing", nfdv1alpha1.NodeFeatureObjNodeNameLabel)
	}
	if nodeName == "" {
		return "", fmt.Errorf("%q label is empty", nfdv1alpha1.NodeFeatureObjNodeNameLabel)
	}
	return nodeName, nil
}

func (c *nfdController) updateOneNode(typ string, obj metav1.Object) {
	nodeName, err := getNodeNameForObj(obj)
	if err != nil {
		klog.ErrorS(err, "failed to determine node name for object", "type", typ, "object", klog.KObj(obj))
		return
	}
	c.updateOneNodeChan <- nodeName
}

func (c *nfdController) isNamespaceSelected(namespace string) bool {
	namespaces, err := c.namespaceLister.list()
	if err != nil {
		klog.ErrorS(err, "failed to query namespaces by the namespace lister")
		return false
	}

	for _, ns := range namespaces {
		if ns.Name == namespace {
			return true
		}
	}

	return false
}

func (c *nfdController) updateAllNodes() {
	select {
	case c.updateAllNodesChan <- struct{}{}:
	default:
	}
}

func (c *nfdController) updateNodeFeatureGroup(nodeFeatureGroup string) {
	c.updateNodeFeatureGroupChan <- nodeFeatureGroup
}

func (c *nfdController) updateAllNodeFeatureGroups() {
	select {
	case c.updateAllNodeFeatureGroupsChan <- struct{}{}:
	default:
	}
}
