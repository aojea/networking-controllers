package loadbalancer

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	utilsnet "k8s.io/utils/net"
)

const (
	controllerName = "fake-loadbalancer-controller"
	maxRetries     = 5
)

// Controller implements a loadbalancer controller that assignes IPs
// to the loadbalancer Services from a predefined IP range
// USED ONLY FOR TESTING
type Controller struct {
	client clientset.Interface
	queue  workqueue.RateLimitingInterface
	// serviceLister is able to list/get services and is populated by the shared informer passed to
	serviceLister corelisters.ServiceLister
	// servicesSynced returns true if the service shared informer has been synced at least once.
	servicesSynced cache.InformerSynced
	// workerLoopPeriod is the time between worker runs. The workers process the queue of service and pod changes.
	workerLoopPeriod time.Duration

	// ip allocation
	mu sync.Mutex
	// TODO dual-stack??
	cidr           *net.IPNet
	ipallocator    sets.String
	serviceTracker map[string]string
}

func NewController(cidr *net.IPNet,
	client clientset.Interface,
	serviceInformer coreinformers.ServiceInformer) *Controller {

	c := &Controller{
		client:           client,
		queue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		cidr:             cidr,
		ipallocator:      sets.NewString(),
		serviceTracker:   map[string]string{},
		workerLoopPeriod: time.Second,
	}
	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.onServiceAdd,
		UpdateFunc: c.onServiceUpdate,
		DeleteFunc: c.onServiceDelete,
	})
	c.serviceLister = serviceInformer.Lister()
	c.servicesSynced = serviceInformer.Informer().HasSynced
	return c
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
	if !cache.WaitForNamedCacheSync(controllerName, stopCh, c.servicesSynced) {
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
// workqueue guarantees that they will not end up processing the same service
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

	err := c.syncServices(eKey.(string))
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

	if c.queue.NumRequeues(key) < maxRetries {
		klog.V(2).InfoS("Error syncing service, retrying", "service", klog.KRef(ns, name), "err", err)
		c.queue.AddRateLimited(key)
		return
	}

	klog.Warningf("Dropping service %q out of the queue: %v", key, err)
	c.queue.Forget(key)
	utilruntime.HandleError(err)
}

func (c *Controller) syncServices(key string) error {
	startTime := time.Now()
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	klog.Infof("Processing sync for service %s on namespace %s ", name, namespace)

	defer func() {
		klog.V(4).Infof("Finished syncing service %s on namespace %s : %v", name, namespace, time.Since(startTime))
	}()

	// Get current Service from the cache
	service, err := c.serviceLister.Services(namespace).Get(name)
	// It´s unlikely that we have an error different that "Not Found Object"
	// because we are getting the object from the informer´s cache
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// service no longer exist or is no longer type loadbalancer
	// release the IP from the allocator and stop tracking the service
	if err != nil || service.Spec.Type != v1.ServiceTypeLoadBalancer {
		// return if we were not tracking this service
		ip := c.getServiceIP(key)
		if ip == "" {
			return nil
		}
		// clear the service status if the service has mutated
		if err == nil {
			service.Status.LoadBalancer = v1.LoadBalancerStatus{}
			_, errUpdate := c.client.CoreV1().Services(namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
			if errUpdate != nil {
				return errUpdate
			}
		}
		klog.Infof("Release IP %s for service %s on namespace %s ", ip, name, namespace)
		c.releaseIP(ip)
		c.deleteService(key)
		return nil
	}
	// service is LoadBalancer check if it needs an IP
	if len(service.Status.LoadBalancer.Ingress) > 0 {
		klog.Infof("Update IP %s for service %s on namespace %s ", service.Status.LoadBalancer.Ingress[0].IP, name, namespace)
		c.addService(key, service.Status.LoadBalancer.Ingress[0].IP)
		return nil
	}
	// assign an IP address to the service
	lbIP, err := c.allocateIP()
	if err != nil {
		return err
	}
	service.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP}}
	_, err = c.client.CoreV1().Services(namespace).UpdateStatus(context.TODO(), service, metav1.UpdateOptions{})
	if err != nil {
		c.releaseIP(lbIP)
		return err
	}
	klog.Infof("Assign IP %s for service %s on namespace %s ", lbIP, name, namespace)
	c.addService(key, lbIP)
	return nil
}

// service tracker

// add or update service
func (c *Controller) addService(key, ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceTracker[key] = ip
	return
}

func (c *Controller) getServiceIP(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serviceTracker[key]
}

func (c *Controller) deleteService(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.serviceTracker, key)

}

// allocator

func (c *Controller) allocateIP() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	max := int(utilsnet.RangeSize(c.cidr))
	if len(c.ipallocator) >= max {
		return "", fmt.Errorf("no free IPs")
	}
	// find a free IPs
	offset := rand.Intn(max)
	var i int
	for i = 0; i < max; i++ {
		at := (offset + i) % max
		ip, err := utilsnet.GetIndexedIP(c.cidr, at)
		if err != nil {
			return "", err
		}
		ipStr := ip.String()
		if !c.ipallocator.Has(ipStr) {
			c.ipallocator.Insert(ipStr)
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no free IPs")
}

func (c *Controller) releaseIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ipallocator.Delete(ip)
}

// handlers

// onServiceUpdate queues the Service for processing.
func (c *Controller) onServiceAdd(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Adding service %s", key)
	c.queue.Add(key)
}

// onServiceUpdate updates the Service Selector in the cache and queues the Service for processing.
func (c *Controller) onServiceUpdate(oldObj, newObj interface{}) {
	oldService := oldObj.(*v1.Service)
	newService := newObj.(*v1.Service)

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

// onServiceDelete queues the Service for processing.
func (c *Controller) onServiceDelete(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}
	klog.V(4).Infof("Deleting service %s", key)
	c.queue.Add(key)
}
