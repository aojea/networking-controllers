package networkpolicy

import (
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	networkinginformers "k8s.io/client-go/informers/networking/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	networkinglisters "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// maxRetries is the number of times a object will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the
	// sequence of delays between successive queuings of an object.
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	controllerName = "kubernetes-networkpolicy-controller"
)

// NewController returns a new *Controller.
func NewController(client clientset.Interface,
	networkpolicyInformer networkinginformers.NetworkPolicyInformer,
	namespaceInformer coreinformers.NamespaceInformer,
	podInformer coreinformers.PodInformer,
) *Controller {
	klog.V(4).Info("Creating event broadcaster")
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: client.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: controllerName})

	c := &Controller{
		client:           client,
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		workerLoopPeriod: time.Second,
	}

	// network policies
	klog.Info("Setting up event handlers for network policies")
	networkpolicyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNetworkPolicyAdd,
		UpdateFunc: c.onNetworkPolicyUpdate,
		DeleteFunc: c.onNetworkPolicyDelete,
	})
	c.networkpolicyLister = networkpolicyInformer.Lister()
	c.networkpolicysSynced = networkpolicyInformer.Informer().HasSynced

	// namespaces
	klog.Info("Setting up event handlers for namespaces")
	namespaceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onNamespaceAdd,
		UpdateFunc: c.onNamespaceUpdate,
		DeleteFunc: c.onNamespaceDelete,
	})

	c.namespaceLister = namespaceInformer.Lister()
	c.namespacesSynced = namespaceInformer.Informer().HasSynced

	// pods
	klog.Info("Setting up event handlers for pods")
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onPodAdd,
		UpdateFunc: c.onPodUpdate,
		DeleteFunc: c.onPodDelete,
	})

	c.podLister = podInformer.Lister()
	c.podsSynced = podInformer.Informer().HasSynced

	c.eventBroadcaster = broadcaster
	c.eventRecorder = recorder

	return c
}

// Controller manages selector-based networkpolicy endpoints.
type Controller struct {
	client           clientset.Interface
	eventBroadcaster record.EventBroadcaster
	eventRecorder    record.EventRecorder

	networkpolicyLister  networkinglisters.NetworkPolicyLister
	networkpolicysSynced cache.InformerSynced
	namespaceLister      corelisters.NamespaceLister
	namespacesSynced     cache.InformerSynced
	podLister            corelisters.PodLister
	podsSynced           cache.InformerSynced

	queue workqueue.RateLimitingInterface

	// workerLoopPeriod is the time between worker runs. The workers process the queue of networkpolicy and pod changes.
	workerLoopPeriod time.Duration
}

// Run will not return until stopCh is closed. workers determines how many
// endpoints will be handled in parallel.
func (c *Controller) Run(workers int, stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting controller %s", controllerName)
	defer klog.Infof("Shutting down controller %s", controllerName)

	// Wait for the caches to be synced
	klog.Info("Waiting for informer caches to sync")
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.networkpolicysSynced, c.namespacesSynced, c.podsSynced) {
		return fmt.Errorf("error syncing cache")
	}

	// Start the workers after the repair loop to avoid races
	klog.Info("Starting workers")
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, c.workerLoopPeriod, stopCh)
	}

	<-stopCh
	return nil
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same networkpolicy
// at the same time.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	eKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(eKey)

	err := c.syncNetworkPolicy(eKey.(string))
	c.handleErr(err, eKey)

	return true
}

func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		c.queue.Forget(key)
		return
	}

	ns, name, keyErr := cache.SplitMetaNamespaceKey(key.(string))
	if keyErr != nil {
		klog.ErrorS(err, "Failed to split meta namespace cache key", "key", key)
	}

	// MetricRequeueServiceCount.WithLabelValues(key.(string)).Inc()

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing networkpolicy, retrying", "networkpolicy", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping network policy %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (c *Controller) syncNetworkPolicy(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for network policy %s on namespace %s ", name, namespace)
	// MetricSyncServiceCount.WithLabelValues(key).Inc()

	defer func() {
		klog.V(4).Infof("Finished syncing network policy %s on namespace %s : %v", name, namespace, time.Since(startTime))
		// MetricSyncServiceLatency.WithLabelValues(key).Observe(time.Since(startTime).Seconds())
	}()

	// Get current Service from the cache
	networkpolicy, err := c.networkpolicyLister.NetworkPolicies(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	klog.Infof("Creating networkpolicy %s on namespace %s", name, namespace)
	klog.Infof("Network Policy %+v", networkpolicy)

	return nil
}

// handlers

// onNetworkPolicyUpdate queues the Service for processing.
func (c *Controller) onNetworkPolicyAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding networkpolicy %s", key)
	c.queue.Add(key)
}

// onNetworkPolicyUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onNetworkPolicyUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*networkingv1.NetworkPolicy)
	newService := newObj.(*networkingv1.NetworkPolicy)

	// don't process resync or objects that are marked for deletion
	if oldService.ResourceVersion == newService.ResourceVersion ||
		!newService.GetDeletionTimestamp().IsZero() {
		return
	}

	key, err := cache.MetaNamespaceKeyFunc(newObj)
	if err == nil {
		c.queue.Add(key)
	}
}

// onNetworkPolicyDelete queues the Service for processing.
func (c *Controller) onNetworkPolicyDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting networkpolicy %s", key)
	c.queue.Add(key)
}

// onNamespaceAdd queues a sync for the relevant Service for a sync
func (c *Controller) onNamespaceAdd(obj interface{}) {
	namespace := obj.(*v1.Namespace)
	if namespace == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onNamespaceAdd()"))
		return
	}
	c.queueServiceForEndpointSlice(namespace)
}

// onNamespaceUpdate queues a sync for the relevant Service for a sync
func (c *Controller) onNamespaceUpdate(prevObj, obj interface{}) {
	prevNamespace := prevObj.(*v1.Namespace)
	namespace := obj.(*v1.Namespace)

	// don't process resync or objects that are marked for deletion
	if prevNamespace.ResourceVersion == namespace.ResourceVersion ||
		!namespace.GetDeletionTimestamp().IsZero() {
		return
	}
	c.queueServiceForEndpointSlice(namespace)
}

// onNamespaceDelete queues a sync for the relevant Service for a sync if the
// EndpointSlice resource version does not match the expected version in the
// namespaceTracker.
func (c *Controller) onNamespaceDelete(obj interface{}) {
	namespace, ok := obj.(*v1.Namespace)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		namespace, ok = tombstone.Obj.(*v1.Namespace)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a EndpointSlice: %#v", obj))
			return
		}
	}

	if namespace != nil {
		//c.queueServiceForEndpointSlice(namespace)
	}
}

// onPodAdd queues a sync for the relevant Service for a sync
func (c *Controller) onPodAdd(obj interface{}) {
	pod := obj.(*v1.Pod)
	if pod == nil {
		utilruntime.HandleError(fmt.Errorf("invalid EndpointSlice provided to onPodAdd()"))
		return
	}
	c.queueServiceForEndpointSlice(pod)
}

// onPodUpdate queues a sync for the relevant Service for a sync
func (c *Controller) onPodUpdate(prevObj, obj interface{}) {
	prevPod := prevObj.(*v1.Pod)
	pod := obj.(*v1.Pod)

	// don't process resync or objects that are marked for deletion
	if prevPod.ResourceVersion == pod.ResourceVersion ||
		!pod.GetDeletionTimestamp().IsZero() {
		return
	}
	c.queueServiceForEndpointSlice(pod)
}

// onPodDelete queues a sync for the relevant Service for a sync if the
// EndpointSlice resource version does not match the expected version in the
// namespaceTracker.
func (c *Controller) onPodDelete(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
			return
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("tombstone contained object that is not a Pod: %#v", obj))
			return
		}
	}

	if pod != nil {
		c.queueServiceForEndpointSlice(pod)
	}
}

// queueServiceForEndpointSlice attempts to queue the corresponding Service for
// the provided EndpointSlice.
func (c *Controller) queueServiceForEndpointSlice(namespace *v1.Pod) {
	key, err := networkpolicyControllerKey(namespace)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for EndpointSlice %+v: %v", namespace, err))
		return
	}

	c.queue.Add(key)
}

// networkpolicyControllerKey returns a controller key for a Service but derived from
// an EndpointSlice.
func networkpolicyControllerKey(namespace *v1.Pod) (string, error) {
	if namespace == nil {
		return "", fmt.Errorf("nil EndpointSlice passed to networkpolicyControllerKey()")
	}

	return fmt.Sprintf("%s/%s", namespace.Namespace, "networkpolicyName"), nil
}
