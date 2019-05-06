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

package logic

import (
	"github.com/golang/glog"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	vpa_types "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1beta2"
	vpa_lister "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/listers/autoscaling.k8s.io/v1beta2"
	"k8s.io/autoscaler/vertical-pod-autoscaler/pkg/target"
	vpa_api_util "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/utils/vpa"
)

// ContainerResources holds resources request for container
type ContainerResources struct {
	Limits   v1.ResourceList
	Requests v1.ResourceList
}

func newContainerResources() ContainerResources {
	return ContainerResources{
		Requests: v1.ResourceList{},
		Limits:   v1.ResourceList{},
	}
}

// RecommendationProvider gets current recommendation, annotations and vpaName for the given pod.
type RecommendationProvider interface {
	GetContainersResourcesForPod(pod *v1.Pod) ([]ContainerResources, vpa_api_util.ContainerToAnnotationsMap, string, error)
}

type recommendationProvider struct {
	vpaLister               vpa_lister.VerticalPodAutoscalerLister
	recommendationProcessor vpa_api_util.RecommendationProcessor
	selectorFetcher         target.VpaTargetSelectorFetcher
}

// NewRecommendationProvider constructs the recommendation provider that list VPAs and can be used to determine recommendations for pods.
func NewRecommendationProvider(vpaLister vpa_lister.VerticalPodAutoscalerLister, recommendationProcessor vpa_api_util.RecommendationProcessor, selectorFetcher target.VpaTargetSelectorFetcher) *recommendationProvider {
	return &recommendationProvider{
		vpaLister:               vpaLister,
		recommendationProcessor: recommendationProcessor,
		selectorFetcher:         selectorFetcher,
	}
}

// getContainersResources returns the recommended resources for each container in the given pod in the same order they are specified in the pod.Spec.
func getContainersResources(pod *v1.Pod, podRecommendation vpa_types.RecommendedPodResources) []ContainerResources {
	resources := make([]ContainerResources, len(pod.Spec.Containers))
	for i, container := range pod.Spec.Containers {
		resources[i] = newContainerResources()

		recommendation := vpa_api_util.GetRecommendationForContainer(container.Name, &podRecommendation)
		if recommendation == nil {
			glog.V(2).Infof("no matching recommendation found for container %s", container.Name)
			continue
		}
		resources[i].Requests = recommendation.Target

		// If user set containers limit and request for a resource to the same value then keep the limit in sync.
		if container.Resources.Limits.Cpu() != nil && container.Resources.Limits.Cpu().Value() != 0 &&
			container.Resources.Requests.Cpu() != nil && *container.Resources.Limits.Cpu() == *container.Resources.Requests.Cpu() {
			resources[i].Limits[v1.ResourceCPU] = *resources[i].Requests.Cpu()
		}

		if container.Resources.Limits.Memory() != nil && container.Resources.Limits.Memory().Value() != 0 &&
			container.Resources.Requests.Memory() != nil && *container.Resources.Limits.Memory() == *container.Resources.Requests.Memory() {
			resources[i].Limits[v1.ResourceMemory] = *resources[i].Requests.Memory()
		}
	}
	return resources
}

func (p *recommendationProvider) getMatchingVPA(pod *v1.Pod) *vpa_types.VerticalPodAutoscaler {
	configs, err := p.vpaLister.VerticalPodAutoscalers(pod.Namespace).List(labels.Everything())
	if err != nil {
		glog.Errorf("failed to get vpa configs: %v", err)
		return nil
	}
	onConfigs := make([]*vpa_api_util.VpaWithSelector, 0)
	for _, vpaConfig := range configs {
		if vpa_api_util.GetUpdateMode(vpaConfig) == vpa_types.UpdateModeOff {
			continue
		}
		selector, err := p.selectorFetcher.Fetch(vpaConfig)
		if err != nil {
			glog.V(3).Infof("skipping VPA object %v because we cannot fetch selector", vpaConfig.Name)
			continue
		}
		onConfigs = append(onConfigs, &vpa_api_util.VpaWithSelector{
			Vpa:      vpaConfig,
			Selector: selector,
		})
	}
	glog.V(2).Infof("Let's choose from %d configs for pod %s/%s", len(onConfigs), pod.Namespace, pod.Name)
	result := vpa_api_util.GetControllingVPAForPod(pod, onConfigs)
	if result != nil {
		return result.Vpa
	}
	return nil
}

// GetContainersResourcesForPod returns recommended request for a given pod, annotations and name of controlling VPA.
// The returned slice corresponds 1-1 to containers in the Pod.
func (p *recommendationProvider) GetContainersResourcesForPod(pod *v1.Pod) ([]ContainerResources, vpa_api_util.ContainerToAnnotationsMap, string, error) {
	glog.V(2).Infof("updating requirements for pod %s.", pod.Name)
	vpaConfig := p.getMatchingVPA(pod)
	if vpaConfig == nil {
		glog.V(2).Infof("no matching VPA found for pod %s", pod.Name)
		return nil, nil, "", nil
	}

	var annotations vpa_api_util.ContainerToAnnotationsMap
	recommendedPodResources := &vpa_types.RecommendedPodResources{}

	if vpaConfig.Status.Recommendation != nil {
		var err error
		recommendedPodResources, annotations, err = p.recommendationProcessor.Apply(vpaConfig.Status.Recommendation, vpaConfig.Spec.ResourcePolicy, vpaConfig.Status.Conditions, pod)
		if err != nil {
			glog.V(2).Infof("cannot process recommendation for pod %s", pod.Name)
			return nil, annotations, vpaConfig.Name, err
		}
	}
	containerResources := getContainersResources(pod, *recommendedPodResources)
	return containerResources, annotations, vpaConfig.Name, nil
}
