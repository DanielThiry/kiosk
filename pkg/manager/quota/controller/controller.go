/*
Copyright 2014 The Kubernetes Authors.

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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	configv1alpha1 "github.com/kiosk-sh/kiosk/pkg/apis/config/v1alpha1"
	"github.com/kiosk-sh/kiosk/pkg/constants"
	"github.com/kiosk-sh/kiosk/pkg/util"

	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubernetes/pkg/controller"
	quota "k8s.io/kubernetes/pkg/quota/v1"
)

// NamespacedResourcesFunc knows how to discover namespaced resources.
type NamespacedResourcesFunc func() ([]*metav1.APIResourceList, error)

// AccountQuotaControllerOptions holds options for creating a quota controller
type AccountQuotaControllerOptions struct {
	// Manager is needed for kubernetes access and cache
	Manager manager.Manager
	// Controls full recalculation of quota usage
	ResyncPeriod controller.ResyncPeriodFunc
	// Maintains evaluators that know how to calculate usage for group resource
	Registry quota.Registry
	// A function that returns the list of resources to ignore
	IgnoredResourcesFunc func() map[schema.GroupResource]struct{}
	// Controls full resync of objects monitored for replenishment.
	ReplenishmentResyncPeriod controller.ResyncPeriodFunc
}

// AccountQuotaController is responsible for tracking quota usage status in the system
type AccountQuotaController struct {
	// Manager has access to the cache and informers
	manager manager.Manager
	// Client is used for accessing kubernetes objects
	client client.Client
	// ResourceQuota objects that need to be synchronized
	queue workqueue.RateLimitingInterface
	// missingUsageQueue holds objects that are missing the initial usage information
	missingUsageQueue workqueue.RateLimitingInterface
	// To allow injection of syncUsage for testing.
	syncHandler func(key string) error
	// function that controls full recalculation of quota usage
	resyncPeriod controller.ResyncPeriodFunc
	// knows how to calculate usage
	registry quota.Registry
	// knows how to monitor all the resources tracked by quota and trigger replenishment
	quotaMonitor *QuotaMonitor
	// controls the workers that process quotas
	// this lock is acquired to control write access to the monitors and ensures that all
	// monitors are synced before the controller can process quotas.
	workerLock sync.RWMutex
}

// NewAccountQuotaController creates a quota controller with specified options
func NewAccountQuotaController(options *AccountQuotaControllerOptions) (*AccountQuotaController, error) {
	// build the account quota controller
	rq := &AccountQuotaController{
		manager:           options.Manager,
		client:            options.Manager.GetClient(),
		queue:             workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "resourcequota_primary"),
		missingUsageQueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "resourcequota_priority"),
		resyncPeriod:      options.ResyncPeriod,
		registry:          options.Registry,
	}
	// set the synchronization handler
	rq.syncHandler = rq.syncResourceQuotaFromKey

	// Observe namespace changes
	namespaceQuotaInformer, err := options.Manager.GetCache().GetInformer(&v1.Namespace{})
	if err != nil {
		return nil, err
	}

	namespaceQuotaInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: rq.enqueueNamespace,
			UpdateFunc: func(old, cur interface{}) {
				oldNamespace := old.(*v1.Namespace)
				newNamespace := cur.(*v1.Namespace)

				if util.GetAccountFromNamespace(oldNamespace) == util.GetAccountFromNamespace(newNamespace) {
					return
				}

				rq.enqueueNamespace(oldNamespace)
				rq.enqueueNamespace(newNamespace)
			},
			DeleteFunc: rq.enqueueNamespace,
		},
		rq.resyncPeriod(),
	)

	// Observe account quota changes
	accountQuotaInformer, err := options.Manager.GetCache().GetInformer(&configv1alpha1.AccountQuota{})
	if err != nil {
		return nil, err
	}

	accountQuotaInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: rq.addQuota,
			UpdateFunc: func(old, cur interface{}) {
				// We are only interested in observing updates to quota.spec to drive updates to quota.status.
				// We ignore all updates to quota.Status because they are all driven by this controller.
				// IMPORTANT:
				// We do not use this function to queue up a full quota recalculation.  To do so, would require
				// us to enqueue all quota.Status updates, and since quota.Status updates involve additional queries
				// that cannot be backed by a cache and result in a full query of a namespace's content, we do not
				// want to pay the price on spurious status updates.  As a result, we have a separate routine that is
				// responsible for enqueue of all resource quotas when doing a full resync (enqueueAll)
				oldAccountQuota := old.(*configv1alpha1.AccountQuota)
				curAccountQuota := cur.(*configv1alpha1.AccountQuota)
				if quota.Equals(oldAccountQuota.Spec.Quota.Hard, curAccountQuota.Spec.Quota.Hard) {
					return
				}
				rq.addQuota(curAccountQuota)
			},
			// This will enter the sync loop and no-op, because the controller has been deleted from the store.
			// Note that deleting a controller immediately after scaling it to 0 will not work. The recommended
			// way of achieving this is by performing a `stop` operation on the controller.
			DeleteFunc: rq.enqueueAccountQuota,
		},
		rq.resyncPeriod(),
	)

	qm := &QuotaMonitor{
		manager:           options.Manager,
		ignoredResources:  options.IgnoredResourcesFunc(),
		resourceChanges:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "resource_quota_controller_resource_changes"),
		resyncPeriod:      options.ReplenishmentResyncPeriod,
		replenishmentFunc: rq.replenishQuota,
		registry:          rq.registry,
	}

	rq.quotaMonitor = qm

	// do initial quota monitor setup.  If we have a discovery failure here, it's ok. We'll discover more resources when a later sync happens.
	resources, err := GetQuotableResources()
	if discovery.IsGroupDiscoveryFailedError(err) {
		utilruntime.HandleError(fmt.Errorf("initial discovery check failure, continuing and counting on future sync update: %v", err))
	} else if err != nil {
		return nil, err
	}

	if err = qm.SyncMonitors(resources); err != nil {
		utilruntime.HandleError(fmt.Errorf("initial monitor sync has error: %v", err))
	}

	// only start quota once all informers synced
	// This shouldn't be necessary, since the informers should be synced directly when getting them
	// rq.informerSyncedFuncs = append(rq.informerSyncedFuncs, qm.IsSynced)

	return rq, nil
}

func (rq *AccountQuotaController) enqueueNamespace(obj interface{}) {
	namespace := obj.(*v1.Namespace)
	account := util.GetAccountFromNamespace(namespace)
	if account == "" {
		return
	}

	// List quotas by account
	quotaList := &configv1alpha1.AccountQuotaList{}
	err := rq.client.List(context.Background(), quotaList, client.MatchingField(constants.IndexByAccount, account))
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to list account quotas: %v", err))
		return
	}

	for _, quota := range quotaList.Items {
		rq.queue.Add(quota.Name)
	}
}

// enqueueAll is called at the fullResyncPeriod interval to force a full recalculation of quota usage statistics
func (rq *AccountQuotaController) enqueueAll() {
	defer klog.V(4).Infof("Resource quota controller queued all resource quota for full calculation of usage")
	accountQuotaList := &configv1alpha1.AccountQuotaList{}
	err := rq.client.List(context.Background(), accountQuotaList)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to enqueue all - error listing resource quotas: %v", err))
		return
	}
	for i := range accountQuotaList.Items {
		key, err := controller.KeyFunc(&accountQuotaList.Items[i])
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %+v: %v", accountQuotaList.Items[i], err))
			continue
		}
		rq.queue.Add(key)
	}
}

// obj could be an *configv1alpha1.AccountQuota, or a DeletionFinalStateUnknown marker item.
func (rq *AccountQuotaController) enqueueAccountQuota(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		klog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	rq.queue.Add(key)
}

func (rq *AccountQuotaController) addQuota(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		klog.Errorf("Couldn't get key for object %+v: %v", obj, err)
		return
	}

	accountQuota := obj.(*configv1alpha1.AccountQuota)

	// if we declared an intent that is not yet captured in status (prioritize it)
	if !apiequality.Semantic.DeepEqual(accountQuota.Spec.Quota.Hard, accountQuota.Status.Total.Hard) {
		rq.missingUsageQueue.Add(key)
		return
	}

	// if we declared a constraint that has no usage (which this controller can calculate, prioritize it)
	for constraint := range accountQuota.Status.Total.Hard {
		if _, usageFound := accountQuota.Status.Total.Used[constraint]; !usageFound {
			matchedResources := []v1.ResourceName{v1.ResourceName(constraint)}
			for _, evaluator := range rq.registry.List() {
				if intersection := evaluator.MatchingResources(matchedResources); len(intersection) > 0 {
					rq.missingUsageQueue.Add(key)
					return
				}
			}
		}
	}

	// no special priority, go in normal recalc queue
	rq.queue.Add(key)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
func (rq *AccountQuotaController) worker(queue workqueue.RateLimitingInterface) func() {
	workFunc := func() bool {
		key, quit := queue.Get()
		if quit {
			return true
		}
		defer queue.Done(key)
		rq.workerLock.RLock()
		defer rq.workerLock.RUnlock()
		err := rq.syncHandler(key.(string))
		if err == nil {
			queue.Forget(key)
			return false
		}
		utilruntime.HandleError(err)
		queue.AddRateLimited(key)
		return false
	}

	return func() {
		for {
			if quit := workFunc(); quit {
				klog.Infof("resource quota controller worker shutting down")
				return
			}
		}
	}
}

// Run begins quota controller using the specified number of workers
func (rq *AccountQuotaController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer rq.queue.ShutDown()

	klog.Infof("Starting resource quota controller")
	defer klog.Infof("Shutting down resource quota controller")

	if rq.quotaMonitor != nil {
		go rq.quotaMonitor.Run(stopCh)
	}

	// This is not necessary since the underlying cache will take care of this
	// if !cache.WaitForNamedCacheSync("resource quota", stopCh, rq.informerSyncedFuncs...) {
	// 	return
	// }

	// the workers that chug through the quota calculation backlog
	for i := 0; i < workers; i++ {
		go wait.Until(rq.worker(rq.queue), time.Second, stopCh)
		go wait.Until(rq.worker(rq.missingUsageQueue), time.Second, stopCh)
	}
	// the timer for how often we do a full recalculation across all quotas
	go wait.Until(func() { rq.enqueueAll() }, rq.resyncPeriod(), stopCh)
	<-stopCh
}

// syncResourceQuotaFromKey syncs a quota key
func (rq *AccountQuotaController) syncResourceQuotaFromKey(key string) (err error) {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing resource quota %q (%v)", key, time.Since(startTime))
	}()

	accountQuota := &configv1alpha1.AccountQuota{}
	err = rq.client.Get(context.Background(), types.NamespacedName{Name: key}, accountQuota)
	if errors.IsNotFound(err) {
		klog.Infof("Resource quota has been deleted %v", key)
		return nil
	}
	if err != nil {
		klog.Infof("Unable to retrieve resource quota %v from store: %v", key, err)
		return err
	}
	return rq.syncResourceQuota(accountQuota)
}

// syncResourceQuota runs a complete sync of resource quota status across all known kinds
func (rq *AccountQuotaController) syncResourceQuota(accountQuota *configv1alpha1.AccountQuota) (err error) {
	// quota is dirty if any part of spec hard limits differs from the status hard limits
	statusLimitsDirty := !apiequality.Semantic.DeepEqual(accountQuota.Spec.Quota.Hard, accountQuota.Status.Total.Hard)

	// dirty tracks if the usage status differs from the previous sync,
	// if so, we send a new usage with latest status
	// if this is our first sync, it will be dirty by default, since we need track usage
	dirty := statusLimitsDirty || accountQuota.Status.Total.Hard == nil || accountQuota.Status.Total.Used == nil
	hardLimits := quota.Add(v1.ResourceList{}, accountQuota.Spec.Quota.Hard)

	// iterate over all quota namespaces and calculate usages
	namespaceList := &v1.NamespaceList{}
	err = rq.client.List(context.Background(), namespaceList, client.MatchingField(constants.IndexByAccount, accountQuota.Spec.Account))
	if err != nil {
		return err
	}

	// ensure set of used values match those that have hard constraints
	hardResources := quota.ResourceNames(hardLimits)
	used := configv1alpha1.AccountQuotasStatusByNamespace{}
	totalUsed := v1.ResourceList{}
	errors := []error{}
	for _, n := range namespaceList.Items {
		newUsage, err := quota.CalculateUsage(n.Name, accountQuota.Spec.Quota.Scopes, hardLimits, rq.registry, accountQuota.Spec.Quota.ScopeSelector)
		if err != nil {
			// if err is non-nil, remember it to return, but continue updating status with any resources in newUsage
			errors = append(errors, err)
		}

		usedList := v1.ResourceList{}
		for _, nsStatus := range accountQuota.Status.Namespaces {
			if nsStatus.Namespace == n.Name && nsStatus.Status.Used != nil {
				usedList = quota.Add(v1.ResourceList{}, nsStatus.Status.Used)
			}
		}

		for key, value := range newUsage {
			usedList[key] = value
		}

		usedList = quota.Mask(usedList, hardResources)
		used = append(used, configv1alpha1.AccountQuotaStatusByNamespace{
			Namespace: n.Name,
			Status: v1.ResourceQuotaStatus{
				Used: usedList,
			},
		})

		totalUsed = quota.Add(totalUsed, usedList)
	}

	// Create a usage object that is based on the quota resource version that will handle updates
	// by default, we preserve the past usage observation, and set hard to the current spec
	usage := accountQuota.DeepCopy()
	usage.Status = configv1alpha1.AccountQuotaStatus{
		Total: v1.ResourceQuotaStatus{
			Hard: hardLimits,
			Used: totalUsed,
		},
		Namespaces: used,
	}

	dirty = dirty || !quota.Equals(usage.Status.Total.Used, accountQuota.Status.Total.Used)

	// there was a change observed by this controller that requires we update quota
	if dirty {
		err = rq.client.Status().Update(context.Background(), usage)
		if err != nil {
			errors = append(errors, err)
		}
	}
	return utilerrors.NewAggregate(errors)
}

// replenishQuota is a replenishment function invoked by a controller to notify that a quota should be recalculated
func (rq *AccountQuotaController) replenishQuota(groupResource schema.GroupResource, namespace string) {
	// check if the quota controller can evaluate this groupResource, if not, ignore it altogether...
	evaluator := rq.registry.Get(groupResource)
	if evaluator == nil {
		return
	}

	// check if this namespace even has a quota...
	accountQuotaList := &configv1alpha1.AccountQuotaList{}
	err := rq.client.List(context.Background(), accountQuotaList)
	if errors.IsNotFound(err) {
		utilruntime.HandleError(fmt.Errorf("quota controller could not find ResourceQuota associated with namespace: %s, could take up to %v before a quota replenishes", namespace, rq.resyncPeriod()))
		return
	}
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error checking to see if namespace %s has any ResourceQuota associated with it: %v", namespace, err))
		return
	}
	if len(accountQuotaList.Items) == 0 {
		return
	}

	// only queue those quotas that are tracking a resource associated with this kind.
	for i := range accountQuotaList.Items {
		accountQuota := &accountQuotaList.Items[i]
		accountQuotaResources := quota.ResourceNames(accountQuota.Status.Total.Hard)
		if intersection := evaluator.MatchingResources(accountQuotaResources); len(intersection) > 0 {
			// TODO: make this support targeted replenishment to a specific kind, right now it does a full recalc on that quota.
			rq.enqueueAccountQuota(accountQuota)
		}
	}
}

// Sync periodically resyncs the controller when new resources are observed from discovery.
func (rq *AccountQuotaController) Sync(discoveryFunc NamespacedResourcesFunc, period time.Duration, stopCh <-chan struct{}) {
	// Something has changed, so track the new state and perform a sync.
	oldResources := make(map[schema.GroupVersionResource]struct{})
	wait.Until(func() {
		// Get the current resource list from discovery.
		newResources, err := GetQuotableResources()
		if err != nil {
			utilruntime.HandleError(err)

			if discovery.IsGroupDiscoveryFailedError(err) && len(newResources) > 0 {
				// In partial discovery cases, don't remove any existing informers, just add new ones
				for k, v := range oldResources {
					newResources[k] = v
				}
			} else {
				// short circuit in non-discovery error cases or if discovery returned zero resources
				return
			}
		}

		// Decide whether discovery has reported a change.
		if reflect.DeepEqual(oldResources, newResources) {
			klog.V(4).Infof("no resource updates from discovery, skipping resource quota sync")
			return
		}

		// Ensure workers are paused to avoid processing events before informers
		// have resynced.
		rq.workerLock.Lock()
		defer rq.workerLock.Unlock()

		// Something has changed, so track the new state and perform a sync.
		if klog.V(2) {
			klog.Infof("syncing resource quota controller with updated resources from discovery: %s", printDiff(oldResources, newResources))
		}

		// Perform the monitor resync and wait for controllers to report cache sync.
		if err := rq.resyncMonitors(newResources); err != nil {
			utilruntime.HandleError(fmt.Errorf("failed to sync resource monitors: %v", err))
			return
		}
		// wait for caches to fill for a while (our sync period).
		// this protects us from deadlocks where available resources changed and one of our informer caches will never fill.
		// informers keep attempting to sync in the background, so retrying doesn't interrupt them.
		// the call to resyncMonitors on the reattempt will no-op for resources that still exist.
		if rq.quotaMonitor != nil && !cache.WaitForNamedCacheSync("resource quota", waitForStopOrTimeout(stopCh, period), rq.quotaMonitor.IsSynced) {
			utilruntime.HandleError(fmt.Errorf("timed out waiting for quota monitor sync"))
			return
		}

		// success, remember newly synced resources
		oldResources = newResources
		klog.V(2).Infof("synced quota controller")
	}, period, stopCh)
}

// printDiff returns a human-readable summary of what resources were added and removed
func printDiff(oldResources, newResources map[schema.GroupVersionResource]struct{}) string {
	removed := sets.NewString()
	for oldResource := range oldResources {
		if _, ok := newResources[oldResource]; !ok {
			removed.Insert(fmt.Sprintf("%+v", oldResource))
		}
	}
	added := sets.NewString()
	for newResource := range newResources {
		if _, ok := oldResources[newResource]; !ok {
			added.Insert(fmt.Sprintf("%+v", newResource))
		}
	}
	return fmt.Sprintf("added: %v, removed: %v", added.List(), removed.List())
}

// waitForStopOrTimeout returns a stop channel that closes when the provided stop channel closes or when the specified timeout is reached
func waitForStopOrTimeout(stopCh <-chan struct{}, timeout time.Duration) <-chan struct{} {
	stopChWithTimeout := make(chan struct{})
	go func() {
		defer close(stopChWithTimeout)
		select {
		case <-stopCh:
		case <-time.After(timeout):
		}
	}()
	return stopChWithTimeout
}

// resyncMonitors starts or stops quota monitors as needed to ensure that all
// (and only) those resources present in the map are monitored.
func (rq *AccountQuotaController) resyncMonitors(resources map[schema.GroupVersionResource]struct{}) error {
	if rq.quotaMonitor == nil {
		return nil
	}

	if err := rq.quotaMonitor.SyncMonitors(resources); err != nil {
		return err
	}
	return nil
}

// GetQuotableResources returns all resources that the quota system should recognize.
// It requires a resource supports the following verbs: 'create','list','delete'
// This function may return both results and an error.  If that happens, it means that the discovery calls were only
// partially successful.  A decision about whether to proceed or not is left to the caller.
func GetQuotableResources() (map[schema.GroupVersionResource]struct{}, error) {
	/*possibleResources, discoveryErr := discoveryFunc()
	if discoveryErr != nil && len(possibleResources) == 0 {
		return nil, fmt.Errorf("failed to discover resources: %v", discoveryErr)
	}
	quotableResources := discovery.FilteredBy(discovery.SupportsAllVerbs{Verbs: []string{"create", "list", "watch", "delete"}}, possibleResources)
	quotableGroupVersionResources, err := discovery.GroupVersionResources(quotableResources)
	if err != nil {
		return nil, fmt.Errorf("Failed to parse resources: %v", err)
	}
	// return the original discovery error (if any) in addition to the list
	return quotableGroupVersionResources, discoveryErr*/

	// We only do pods for now
	return map[schema.GroupVersionResource]struct{}{
		schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}: struct{}{},
	}, nil
}
