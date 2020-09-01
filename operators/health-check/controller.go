package healthcheckoperator

import (
	ctx "context"
	"fmt"
	"reflect"
	"strings"
	"time"

	log "github.com/hashicorp/go-hclog"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller struct defines how a controller should encapsulate
// logging, client connectivity, informing (list and watching)
// queueing, and handling of resource changes
// TODO: right now we only support a single namespace or "metav1.NamespaceAll"
type Controller struct {
	Log        log.Logger
	Clientset  kubernetes.Interface
	Queue      workqueue.RateLimitingInterface
	Informer   cache.SharedIndexInformer
	Handle     Handler
	MaxRetries int
	Namespace  string
}

func (c *Controller) setupInformer() {
	c.Informer = cache.NewSharedIndexInformer(
		// the ListWatch contains two different functions that our
		// informer requires: ListFunc to take care of listing and watching
		// the resources we want to handle
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = "consul" //=consul.hashicorp.com/connect-inject"
				// list all of the pods (core resource) in the k8s namespace
				return c.Clientset.CoreV1().Pods(c.Namespace).List(ctx.Background(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = "consul" //=consul.hashicorp.com/connect-inject"
				// watch all of the pods which match Consul labels in k8s namespace
				return c.Clientset.CoreV1().Pods(c.Namespace).Watch(ctx.Background(), options)
			},
		},
		&corev1.Pod{}, // the target type (Pod)
		0,             // no resync (period of 0)
		cache.Indexers{},
	)
}

func (c *Controller) setupWorkQueue() {
	// create a new queue so that when the informer gets a resource that is either
	// a result of listing or watching, we can add an idenfitying key to the queue
	// so that it can be handled in the handler
	// The queue will be indexed via keys in the format of :  OPTION/namespace/resource
	// where OPTION will be one of ADD/UPDATE/CREATE
	c.Queue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
}

func (c *Controller) addEventHandlers() {
	// add event handlers to handle the three types of events for resources:
	//  - adding new resources
	//  - updating existing resources
	//  - deleting resources
	c.Informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// AddFunc is a no-op as we handle ObjectCreate path on the UpdateFunc
			return
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*corev1.Pod)
			oldPod := oldObj.(*corev1.Pod)

			if reflect.DeepEqual(oldObj, newObj) == false {
				c.Log.Info("pod was updated : " + newPod.Name)
			} else {
				c.Log.Info("pod was not updated " + newPod.Name)
				return
			}

			// First we check if this is a transition from Pending to Running, at this point
			// we have a Pod scheduled and running on a host so we have a hostIP that we can
			// reference.
			if oldPod.Status.Phase == corev1.PodPending && newPod.Status.Phase == corev1.PodRunning {
				// This is the ObjectCreate path
				key, err := cache.MetaNamespaceKeyFunc(newObj)
				c.Log.Info("Add Pod: %s", key)
				if err == nil {
					c.Queue.Add("ADD/" + key)
				}
				// We return here because due to startup timing on probes there is a case where we receive
				// the failed readiness probe before processing the transition from Pending to Running, in which case
				// the Pending->Running transition will be queued, followed by an Update in the processNextItem() function
				// which has the effect of setting the health status to the current state
				return
			}
			// We will only process events for PodRunning Pods
			if newPod.Status.Phase == corev1.PodRunning {
				// Only queue events which satisfy the condition of a pod Status Condition transition
				// from Ready/NotReady or NotReady/Ready
				oldPodStatus := corev1.ConditionTrue
				newPodStatus := corev1.ConditionTrue
				// In this context "Ready" is the name of the Condition field and not the actual Status
				for _, y := range oldPod.Status.Conditions {
					if y.Type == "Ready" {
						oldPodStatus = y.Status
					}
				}
				for _, y := range newPod.Status.Conditions {
					if y.Type == "Ready" {
						newPodStatus = y.Status
					}
				}
				// If the Pod Status has changed, we queue the newObj and we will know based on the condition status
				// whether or not this is an update TO or FROM healthy in the event handler
				if oldPodStatus != newPodStatus {
					key, err := cache.MetaNamespaceKeyFunc(newObj)
					c.Log.Info("Update pod: %s", key)
					if err == nil {
						c.Queue.Add("UPDATE/" + key)
					}
				}

			}
		},
		DeleteFunc: func(obj interface{}) {
			// Deletion is handled by the connect-inject webhook!
			return
		},
	})
}

// Run is the main path of execution for the controller loop
func (c *Controller) Run(stopCh <-chan struct{}) {
	c.Log.Debug("Controller.Run: initializing")
	// Setup the Informer
	c.setupInformer()
	// Next setup the work queue
	c.setupWorkQueue()
	// Next add eventHandlers, these are responsible for defining Create/Update/Delete functionality
	c.addEventHandlers()

	// handle a panic with logging and exiting
	defer utilruntime.HandleCrash()
	// block new items in the Queue in case of shutdown, drain the queue and exit
	defer c.Queue.ShutDown()

	// run the Informer to start listing and watching resources
	go c.Informer.Run(stopCh)

	// do the initial synchronization (one time) to populate resources
	if !cache.WaitForCacheSync(stopCh, c.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("error syncing cache"))
		return
	}
	// run the runWorker method every second with a stop channel
	wait.Until(c.runWorker, time.Second, stopCh)
}

// HasSynced allows us to satisfy the Controller interface
// by wiring up the Informer's HasSynced method to it
func (c *Controller) HasSynced() bool {
	return c.Informer.HasSynced()
}

// runWorker executes the loop to process new items added to the Queue
func (c *Controller) runWorker() {
	c.Log.Debug("Controller.runWorker: starting")
	// invoke processNextItem to fetch and consume the next change
	// to a watched or listed resource
	for c.processNextItem() {
		c.Log.Debug("Controller.runWorker: processing next item")
	}
	c.Log.Debug("Controller.runWorker: completed")
}

// processNextItem retrieves each Queued item and takes the
// necessary Handle action based off of if the item was
// created or deleted
func (c *Controller) processNextItem() bool {
	c.Log.Debug("Controller.processNextItem: start")

	// fetch the next item (blocking) from the Queue to process or
	// if a shutdown is requested then return out of this to stop
	// processing
	key, quit := c.Queue.Get()
	if quit {
		return false
	}
	// Key format is as follows :  CREATE/namespace/name, DELETE/namespace/name, UPDATE/namespace/name
	// also keep track if this is an Add
	create := true
	formattedKey := strings.Split(key.(string), "/")
	if formattedKey[0] != "ADD" {
		create = false
	}
	keyRaw := strings.Join(formattedKey[1:], "/")

	// take the string key and get the object out of the indexer
	//
	// item will contain the complex object for the resource and
	// exists is a bool that'll indicate whether or not the
	// resource was created (true) or deleted (false)
	//
	// if there is an error in getting the key from the index
	// then we want to retry this particular Queue key a certain
	// number of times (c.MaxRetries) before we forget the Queue key
	// and throw an error
	item, exists, err := c.Informer.GetIndexer().GetByKey(keyRaw)
	if err != nil {
		if c.Queue.NumRequeues(key) < c.MaxRetries {
			c.Log.Info("controller.processNextItem: Failed processing item with key %s with error %v, retrying", key, err)
			c.Queue.AddRateLimited(key)
		} else {
			c.Log.Error("controller.processNextItem: Failed processing item with key %s with error %v, no more retries", key, err)
			c.Queue.Forget(key)
			utilruntime.HandleError(err)
		}
	}

	// if the object does exist that indicates that the object
	// was created or updated so run the ObjectCreated/ObjectUpdated method
	// dequeue the key to indicate success, requeue it on failure
	if exists {
		// This is a Pod Create
		if create == true {
			c.Log.Info("controller.processNextItem: object create detected: %s", keyRaw)
			err = c.Handle.ObjectCreated(item)
			if err == nil {
				c.Log.Info("controller.processNextItem: object update as part of ObjectCreate: %s", keyRaw)
				err = c.Handle.ObjectUpdated(item)
			}
		} else {
			// This is a Pod Status Update
			c.Log.Info("controller.processNextItem: object update detected: %s", keyRaw)
			err = c.Handle.ObjectUpdated(item)
		}
		if err == nil {
			// Indicates success
			c.Queue.Forget(key)
		} else if c.Queue.NumRequeues(key) < c.MaxRetries {
			c.Log.Error("unable to process request, retrying")
			c.Queue.AddRateLimited(key)
		}
	}
	if err == nil {
		// marking Done removes the key from the queue entirely
		c.Queue.Done(key)
	}
	// keep the worker loop running by returning true
	return true
}
