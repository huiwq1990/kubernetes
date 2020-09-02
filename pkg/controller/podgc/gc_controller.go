/*
Copyright 2015 The Kubernetes Authors.

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

package podgc

import (
	"sort"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-base/metrics/prometheus/ratelimiter"

	"k8s.io/klog"
)

const (
	// gcCheckPeriod defines frequency of running main controller loop
	gcCheckPeriod = 20 * time.Second
	// quarantineTime defines how long Orphaned GC waits for nodes to show up
	// in an informer before issuing a GET call to check if they are truly gone
	quarantineTime = 40 * time.Second
)

type PodGCController struct {
	kubeClient clientset.Interface

	podLister        corelisters.PodLister
	podListerSynced  cache.InformerSynced
	nodeLister       corelisters.NodeLister
	nodeListerSynced cache.InformerSynced

	nodeQueue workqueue.DelayingInterface
	//调用apiserver删除对应pod的接口
	deletePod              func(namespace, name string) error
	//对应--terminated-pod-gc-threshold的配置，默认为12500
	terminatedPodThreshold int
}
//把相关的PodGCController元素进行赋值
func NewPodGC(kubeClient clientset.Interface, podInformer coreinformers.PodInformer,
	nodeInformer coreinformers.NodeInformer, terminatedPodThreshold int) *PodGCController {
	if kubeClient != nil && kubeClient.CoreV1().RESTClient().GetRateLimiter() != nil {
		ratelimiter.RegisterMetricAndTrackRateLimiterUsage("gc_controller", kubeClient.CoreV1().RESTClient().GetRateLimiter())
	}
	gcc := &PodGCController{
		kubeClient:             kubeClient,
		terminatedPodThreshold: terminatedPodThreshold,
		podLister:              podInformer.Lister(),
		podListerSynced:        podInformer.Informer().HasSynced,
		nodeLister:             nodeInformer.Lister(),
		nodeListerSynced:       nodeInformer.Informer().HasSynced,
		nodeQueue:              workqueue.NewNamedDelayingQueue("orphaned_pods_nodes"),
		deletePod: func(namespace, name string) error {
			klog.Infof("PodGC is force deleting Pod: %v/%v", namespace, name)
			// 表示立即删除pod，没有grace period
			return kubeClient.CoreV1().Pods(namespace).Delete(name, metav1.NewDeleteOptions(0))
		},
	}

	return gcc
}

func (gcc *PodGCController) Run(stop <-chan struct{}) {
	defer utilruntime.HandleCrash()

	klog.Infof("Starting GC controller")
	defer gcc.nodeQueue.ShutDown()
	defer klog.Infof("Shutting down GC controller")

	if !cache.WaitForNamedCacheSync("GC", stop, gcc.podListerSynced, gcc.nodeListerSynced) {
		return
	}

	go wait.Until(gcc.gc, gcCheckPeriod, stop)

	<-stop
}

func (gcc *PodGCController) gc() {
	// 获取所有的pod，不设置过滤
	pods, err := gcc.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Error while listing all pods: %v", err)
		return
	}
	// 获取所有的node，不设置过滤
	nodes, err := gcc.nodeLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Error while listing all nodes: %v", err)
		return
	}
	// 如果terminatedPodThreshold大于0，则调用gcc.gcTerminated(pods)回收那些超出Threshold的Pods。
	if gcc.terminatedPodThreshold > 0 {
		gcc.gcTerminated(pods)
	}
	gcc.gcOrphaned(pods, nodes)
	gcc.gcUnscheduledTerminating(pods)
}

func isPodTerminated(pod *v1.Pod) bool {
	if phase := pod.Status.Phase; phase != v1.PodPending && phase != v1.PodRunning && phase != v1.PodUnknown {
		return true
	}
	return false
}
// gcTerminated删除超出阈值的pods的删除动作是并行的，
// 通过sync.WaitGroup等待所有对应的pods删除完成后，gcTerminated才会结束返回，才能开始后面的gcOrphaned.
func (gcc *PodGCController) gcTerminated(pods []*v1.Pod) {
	terminatedPods := []*v1.Pod{}
	for _, pod := range pods {
		if isPodTerminated(pod) {
			terminatedPods = append(terminatedPods, pod)
		}
	}

	terminatedPodCount := len(terminatedPods)
	deleteCount := terminatedPodCount - gcc.terminatedPodThreshold

	if deleteCount > terminatedPodCount {
		deleteCount = terminatedPodCount
	}
	// 如果terminatedPod数量没有超过阈值，直接返回
	if deleteCount <= 0 {
		return
	}

	klog.Infof("garbage collecting %v pods", deleteCount)
	// sort only when necessary
	sort.Sort(byCreationTimestamp(terminatedPods))
	var wait sync.WaitGroup
	for i := 0; i < deleteCount; i++ {
		wait.Add(1)
		go func(namespace string, name string) {
			defer wait.Done()
			if err := gcc.deletePod(namespace, name); err != nil {
				// ignore not founds
				defer utilruntime.HandleError(err)
			}
		}(terminatedPods[i].Namespace, terminatedPods[i].Name)
	}
	wait.Wait()
}
// 处理pod所属的node不存在情况
// gcOrphaned deletes pods that are bound to nodes that don't exist.
func (gcc *PodGCController) gcOrphaned(pods []*v1.Pod, nodes []*v1.Node) {
	klog.V(4).Infof("GC'ing orphaned")
	// nodename列表
	existingNodeNames := sets.NewString()
	for _, node := range nodes {
		existingNodeNames.Insert(node.Name)
	}
	// pod所属的node不存在
	// Add newly found unknown nodes to quarantine
	for _, pod := range pods {
		if pod.Spec.NodeName != "" && !existingNodeNames.Has(pod.Spec.NodeName) {
			gcc.nodeQueue.AddAfter(pod.Spec.NodeName, quarantineTime)
		}
	}
	// 判断node是否真正删除
	// Check if nodes are still missing after quarantine period
	deletedNodesNames, quit := gcc.discoverDeletedNodes(existingNodeNames)
	if quit {
		return
	}
	// Delete orphaned pods
	for _, pod := range pods {
		if !deletedNodesNames.Has(pod.Spec.NodeName) {
			continue
		}
		klog.V(2).Infof("Found orphaned Pod %v/%v assigned to the Node %v. Deleting.", pod.Namespace, pod.Name, pod.Spec.NodeName)
		if err := gcc.deletePod(pod.Namespace, pod.Name); err != nil {
			utilruntime.HandleError(err)
		} else {
			klog.V(0).Infof("Forced deletion of orphaned Pod %v/%v succeeded", pod.Namespace, pod.Name)
		}
	}
}
// 确认node释放真的不存在
func (gcc *PodGCController) discoverDeletedNodes(existingNodeNames sets.String) (sets.String, bool) {
	deletedNodesNames := sets.NewString()
	for gcc.nodeQueue.Len() > 0 {
		item, quit := gcc.nodeQueue.Get()
		if quit {
			return nil, true
		}
		nodeName := item.(string)
		if !existingNodeNames.Has(nodeName) {
			exists, err := gcc.checkIfNodeExists(nodeName)
			switch {
			case err != nil:
				klog.Errorf("Error while getting node %q: %v", nodeName, err)
				// Node will be added back to the queue in the subsequent loop if still needed
			case !exists:
				deletedNodesNames.Insert(nodeName)
			}
		}
		gcc.nodeQueue.Done(item)
	}
	return deletedNodesNames, false
}

func (gcc *PodGCController) checkIfNodeExists(name string) (bool, error) {
	_, fetchErr := gcc.kubeClient.CoreV1().Nodes().Get(name, metav1.GetOptions{})
	if errors.IsNotFound(fetchErr) {
		return false, nil
	}
	return fetchErr == nil, fetchErr
}
//删除那些terminating并且还没调度到某个node的pods
// gcUnscheduledTerminating deletes pods that are terminating and haven't been scheduled to a particular node.
func (gcc *PodGCController) gcUnscheduledTerminating(pods []*v1.Pod) {
	klog.V(4).Infof("GC'ing unscheduled pods which are terminating.")

	for _, pod := range pods {
		if pod.DeletionTimestamp == nil || len(pod.Spec.NodeName) > 0 {
			continue
		}

		klog.V(2).Infof("Found unscheduled terminating Pod %v/%v not assigned to any Node. Deleting.", pod.Namespace, pod.Name)
		if err := gcc.deletePod(pod.Namespace, pod.Name); err != nil {
			utilruntime.HandleError(err)
		} else {
			klog.V(0).Infof("Forced deletion of unscheduled terminating Pod %v/%v succeeded", pod.Namespace, pod.Name)
		}
	}
}

// byCreationTimestamp sorts a list by creation timestamp, using their names as a tie breaker.
type byCreationTimestamp []*v1.Pod

func (o byCreationTimestamp) Len() int      { return len(o) }
func (o byCreationTimestamp) Swap(i, j int) { o[i], o[j] = o[j], o[i] }

func (o byCreationTimestamp) Less(i, j int) bool {
	if o[i].CreationTimestamp.Equal(&o[j].CreationTimestamp) {
		return o[i].Name < o[j].Name
	}
	return o[i].CreationTimestamp.Before(&o[j].CreationTimestamp)
}
