/*
Copyright 2022.

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

package controllers

import (
	"context"
	"fmt"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/go-logr/logr"
	cinderv1beta1 "github.com/openstack-k8s-operators/cinder-operator/api/v1beta1"
	"github.com/openstack-k8s-operators/cinder-operator/pkg/cinder"
	cinderapi "github.com/openstack-k8s-operators/cinder-operator/pkg/cinderapi"
	keystonev1 "github.com/openstack-k8s-operators/keystone-operator/api/v1beta1"
	keystone "github.com/openstack-k8s-operators/keystone-operator/pkg/external"
	"github.com/openstack-k8s-operators/lib-common/modules/common"
	"github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/configmap"
	"github.com/openstack-k8s-operators/lib-common/modules/common/deployment"
	"github.com/openstack-k8s-operators/lib-common/modules/common/endpoint"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/labels"
	"github.com/openstack-k8s-operators/lib-common/modules/common/secret"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
)

// GetClient -
func (r *CinderAPIReconciler) GetClient() client.Client {
	return r.Client
}

// GetKClient -
func (r *CinderAPIReconciler) GetKClient() kubernetes.Interface {
	return r.Kclient
}

// GetLogger -
func (r *CinderAPIReconciler) GetLogger() logr.Logger {
	return r.Log
}

// GetScheme -
func (r *CinderAPIReconciler) GetScheme() *runtime.Scheme {
	return r.Scheme
}

// CinderAPIReconciler reconciles a CinderAPI object
type CinderAPIReconciler struct {
	client.Client
	Kclient kubernetes.Interface
	Log     logr.Logger
	Scheme  *runtime.Scheme
}

var (
	keystoneServices = []map[string]string{
		{
			"type": cinder.ServiceTypeV2,
			"name": cinder.ServiceNameV2,
			"desc": "Cinder V2 Service",
		},
		{
			"type": cinder.ServiceTypeV3,
			"name": cinder.ServiceNameV3,
			"desc": "Cinder V3 Service",
		},
	}
)

//+kubebuilder:rbac:groups=cinder.openstack.org,resources=cinderapis,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cinder.openstack.org,resources=cinderapis/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cinder.openstack.org,resources=cinderapis/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;create;update;patch;delete;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;create;update;patch;delete;watch
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;create;update;patch;delete;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;create;update;patch;delete;watch

// Reconcile -
func (r *CinderAPIReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// Fetch the CinderAPI instance
	instance := &cinderv1beta1.CinderAPI{}
	err := r.Client.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected.
			// For additional cleanup logic use finalizers. Return and don't requeue.
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	//
	// initialize status
	//
	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.List{}
	}
	if instance.Status.Hash == nil {
		instance.Status.Hash = map[string]string{}
	}
	if instance.Status.APIEndpoints == nil {
		instance.Status.APIEndpoints = map[string]map[string]string{}
	}
	if instance.Status.ServiceIDs == nil {
		instance.Status.ServiceIDs = map[string]string{}
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always patch the instance status when exiting this function so we can persist any changes.
	defer func() {
		if err := helper.SetAfter(instance); err != nil {
			util.LogErrorForObject(helper, err, "Set after and calc patch/diff", instance)
		}

		if changed := helper.GetChanges()["status"]; changed {
			patch := client.MergeFrom(helper.GetBeforeObject())

			if err := r.Status().Patch(ctx, instance, patch); err != nil && !k8s_errors.IsNotFound(err) {
				util.LogErrorForObject(helper, err, "Update status", instance)
			}
		}
	}()

	// Handle service delete
	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, instance, helper)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, instance, helper)
}

// SetupWithManager sets up the controller with the Manager.
func (r *CinderAPIReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// watch for configmap where the CM owner label AND the CR.Spec.ManagingCrName label matches
	configMapFn := func(o client.Object) []reconcile.Request {
		result := []reconcile.Request{}

		// get all API CRs
		apis := &cinderv1beta1.CinderAPIList{}
		listOpts := []client.ListOption{
			client.InNamespace(o.GetNamespace()),
		}
		if err := r.Client.List(context.Background(), apis, listOpts...); err != nil {
			r.Log.Error(err, "Unable to retrieve API CRs %v")
			return nil
		}

		label := o.GetLabels()
		// TODO: Just trying to verify that the CM is owned by this CR's managing CR
		if l, ok := label[labels.GetOwnerNameLabelSelector(labels.GetGroupLabel(cinder.ServiceName))]; ok {
			for _, cr := range apis.Items {
				// return reconcil event for the CR where the CM owner label AND the parentCinderName matches
				if l == cinder.GetOwningCinderName(&cr) {
					// return namespace and Name of CR
					name := client.ObjectKey{
						Namespace: o.GetNamespace(),
						Name:      cr.Name,
					}
					r.Log.Info(fmt.Sprintf("ConfigMap object %s and CR %s marked with label: %s", o.GetName(), cr.Name, l))
					result = append(result, reconcile.Request{NamespacedName: name})
				}
			}
		}
		if len(result) > 0 {
			return result
		}
		return nil
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&cinderv1beta1.CinderAPI{}).
		Owns(&keystonev1.KeystoneService{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&routev1.Route{}).
		Owns(&corev1.Service{}).
		// watch the config CMs we don't own
		Watches(&source.Kind{Type: &corev1.ConfigMap{}},
			handler.EnqueueRequestsFromMapFunc(configMapFn)).
		Complete(r)
}

func (r *CinderAPIReconciler) reconcileDelete(ctx context.Context, instance *cinderv1beta1.CinderAPI, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info("Reconciling Service delete")

	// It's possible to get here before the endpoints have been set in the status, so check for this
	if instance.Status.APIEndpoints != nil {
		for _, ksSvc := range keystoneServices {
			ksSvcSpec := keystonev1.KeystoneServiceSpec{
				ServiceType:        ksSvc["type"],
				ServiceName:        ksSvc["name"],
				ServiceDescription: ksSvc["desc"],
				Enabled:            true,
				APIEndpoints:       instance.Status.APIEndpoints[ksSvc["name"]],
				ServiceUser:        instance.Spec.ServiceUser,
				Secret:             instance.Spec.Secret,
				PasswordSelector:   instance.Spec.PasswordSelectors.Service,
			}

			ksSvcObj := keystone.NewKeystoneService(ksSvcSpec, instance.Namespace, map[string]string{}, 10)

			err := ksSvcObj.Delete(ctx, helper)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Service is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(instance, helper.GetFinalizer())
	r.Log.Info("Reconciled Service delete successfully")
	if err := r.Update(ctx, instance); err != nil && !k8s_errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *CinderAPIReconciler) reconcileInit(
	ctx context.Context,
	instance *cinderv1beta1.CinderAPI,
	helper *helper.Helper,
	serviceLabels map[string]string,
) (ctrl.Result, error) {
	r.Log.Info("Reconciling Service init")

	//
	// expose the service (create service, route and return the created endpoint URLs)
	//

	// V2
	adminEndpointData := endpoint.EndpointData{
		Port: cinder.CinderAdminPort,
		Path: "/v2/%(project_id)s",
	}
	publicEndpointData := endpoint.EndpointData{
		Port: cinder.CinderPublicPort,
		Path: "/v2/%(project_id)s",
	}
	internalEndpointData := endpoint.EndpointData{
		Port: cinder.CinderInternalPort,
		Path: "/v2/%(project_id)s",
	}
	data := map[endpoint.Endpoint]endpoint.EndpointData{
		endpoint.EndpointAdmin:    adminEndpointData,
		endpoint.EndpointPublic:   publicEndpointData,
		endpoint.EndpointInternal: internalEndpointData,
	}

	apiEndpointsV2, ctrlResult, err := endpoint.ExposeEndpoints(
		ctx,
		helper,
		cinder.ServiceName,
		serviceLabels,
		data,
	)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	//
	// Update instance status with service endpoint url from route host information for v2
	//
	// TODO: need to support https default here
	if instance.Status.APIEndpoints == nil {
		instance.Status.APIEndpoints = map[string]map[string]string{}
	}
	instance.Status.APIEndpoints[cinder.ServiceNameV2] = apiEndpointsV2
	// V2 - end

	// V3
	adminEndpointData = endpoint.EndpointData{
		Port: cinder.CinderAdminPort,
		Path: "/v3/%(project_id)s",
	}
	publicEndpointData = endpoint.EndpointData{
		Port: cinder.CinderPublicPort,
		Path: "/v3/%(project_id)s",
	}
	internalEndpointData = endpoint.EndpointData{
		Port: cinder.CinderInternalPort,
		Path: "/v3/%(project_id)s",
	}
	data = map[endpoint.Endpoint]endpoint.EndpointData{
		endpoint.EndpointAdmin:    adminEndpointData,
		endpoint.EndpointPublic:   publicEndpointData,
		endpoint.EndpointInternal: internalEndpointData,
	}

	apiEndpointsV3, ctrlResult, err := endpoint.ExposeEndpoints(
		ctx,
		helper,
		cinder.ServiceName,
		serviceLabels,
		data,
	)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	//
	// Update instance status with service endpoint url from route host information for v2
	//
	// TODO: need to support https default here
	instance.Status.APIEndpoints[cinder.ServiceNameV3] = apiEndpointsV3
	// V3 - end

	// expose service - end

	//
	// create users and endpoints - https://docs.openstack.org/Cinder/latest/install/install-rdo.html#configure-user-and-endpoints
	// TODO: rework this
	//
	_, _, err = secret.GetSecret(ctx, helper, instance.Spec.Secret, instance.Namespace)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("OpenStack secret %s not found", instance.Spec.Secret)
		}
		return ctrl.Result{}, err
	}

	if instance.Status.ServiceIDs == nil {
		instance.Status.ServiceIDs = map[string]string{}
	}

	for _, ksSvc := range keystoneServices {
		ksSvcSpec := keystonev1.KeystoneServiceSpec{
			ServiceType:        ksSvc["type"],
			ServiceName:        ksSvc["name"],
			ServiceDescription: ksSvc["desc"],
			Enabled:            true,
			APIEndpoints:       instance.Status.APIEndpoints[ksSvc["name"]],
			ServiceUser:        instance.Spec.ServiceUser,
			Secret:             instance.Spec.Secret,
			PasswordSelector:   instance.Spec.PasswordSelectors.Service,
		}

		ksSvcObj := keystone.NewKeystoneService(ksSvcSpec, instance.Namespace, serviceLabels, 10)
		ctrlResult, err = ksSvcObj.CreateOrPatch(ctx, helper)
		if err != nil {
			return ctrlResult, err
		} else if (ctrlResult != ctrl.Result{}) {
			return ctrlResult, nil
		}

		instance.Status.ServiceIDs[ksSvc["name"]] = ksSvcObj.GetServiceID()
	}

	r.Log.Info("Reconciled Service init successfully")
	return ctrl.Result{}, nil
}

func (r *CinderAPIReconciler) reconcileNormal(ctx context.Context, instance *cinderv1beta1.CinderAPI, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info("Reconciling Service")

	// If the service object doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(instance, helper.GetFinalizer())
	// Register the finalizer immediately to avoid orphaning resources on delete
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// ConfigMap
	configMapVars := make(map[string]env.Setter)

	//
	// check for required OpenStack secret holding passwords for service/admin user and add hash to the vars map
	//
	ospSecret, hash, err := secret.GetSecret(ctx, helper, instance.Spec.Secret, instance.Namespace)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("OpenStack secret %s not found", instance.Spec.Secret)
		}
		return ctrl.Result{}, err
	}
	configMapVars[ospSecret.Name] = env.SetValue(hash)
	// run check OpenStack secret - end

	//
	// Create ConfigMaps required as input for the Service and calculate an overall hash of hashes
	//

	//
	// create custom Configmap for this cinder volume service
	//
	err = r.generateServiceConfigMaps(ctx, helper, instance, &configMapVars)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Create ConfigMaps - end

	//
	// check for required Cinder config maps that should have been created by ManagingCr
	//

	parentCinderName := cinder.GetOwningCinderName(instance)

	configMaps := []string{
		fmt.Sprintf("%s-scripts", parentCinderName),     //ScriptsConfigMap
		fmt.Sprintf("%s-config-data", parentCinderName), //ConfigMap
	}

	_, err = configmap.GetConfigMaps(ctx, helper, instance, configMaps, instance.Namespace, &configMapVars)
	if err != nil {
		if k8s_errors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Second * 10}, fmt.Errorf("Could not find all config maps for parent Cinder CR %s", parentCinderName)
		}
		return ctrl.Result{}, err
	}

	// run check Cinder ManagingCr config maps - end

	//
	// create hash over all the different input resources to identify if any those changed
	// and a restart/recreate is required.
	//
	inputHash, err := r.createHashOfInputHashes(ctx, instance, configMapVars)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Create ConfigMaps and Secrets - end

	//
	// TODO check when/if Init, Update, or Upgrade should/could be skipped
	//

	serviceLabels := map[string]string{
		common.AppSelector: cinder.ServiceName,
	}

	// Handle service init
	ctrlResult, err := r.reconcileInit(ctx, instance, helper, serviceLabels)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Handle service update
	ctrlResult, err = r.reconcileUpdate(ctx, instance, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Handle service upgrade
	ctrlResult, err = r.reconcileUpgrade(ctx, instance, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	//
	// normal reconcile tasks
	//

	// Define a new Deployment object
	depl := deployment.NewDeployment(
		cinderapi.Deployment(instance, inputHash, serviceLabels),
		5,
	)

	ctrlResult, err = depl.CreateOrPatch(ctx, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}
	instance.Status.ReadyCount = depl.GetDeployment().Status.ReadyReplicas
	// create Deployment - end

	r.Log.Info("Reconciled Service successfully")
	return ctrl.Result{}, nil
}

func (r *CinderAPIReconciler) reconcileUpdate(ctx context.Context, instance *cinderv1beta1.CinderAPI, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info("Reconciling Service update")

	// TODO: should have minor update tasks if required
	// - delete dbsync hash from status to rerun it?

	r.Log.Info("Reconciled Service update successfully")
	return ctrl.Result{}, nil
}

func (r *CinderAPIReconciler) reconcileUpgrade(ctx context.Context, instance *cinderv1beta1.CinderAPI, helper *helper.Helper) (ctrl.Result, error) {
	r.Log.Info("Reconciling Service upgrade")

	// TODO: should have major version upgrade tasks
	// -delete dbsync hash from status to rerun it?

	r.Log.Info("Reconciled Service upgrade successfully")
	return ctrl.Result{}, nil
}

//
// generateServiceConfigMaps - create custom configmap to hold service-specific config
// TODO add DefaultConfigOverwrite
//
func (r *CinderAPIReconciler) generateServiceConfigMaps(
	ctx context.Context,
	h *helper.Helper,
	instance *cinderv1beta1.CinderAPI,
	envVars *map[string]env.Setter,
) error {
	//
	// create custom Configmap for cinder-api-specific config input
	// - %-config-data configmap holding custom config for the service's cinder.conf
	//

	cmLabels := labels.GetLabels(instance, labels.GetGroupLabel(cinder.ServiceName), map[string]string{})

	// customData hold any customization for the service.
	// custom.conf is going to be merged into /etc/cinder/conder.conf
	// TODO: make sure custom.conf can not be overwritten
	customData := map[string]string{common.CustomServiceConfigFileName: instance.Spec.CustomServiceConfig}

	for key, data := range instance.Spec.DefaultConfigOverwrite {
		customData[key] = data
	}

	customData[common.CustomServiceConfigFileName] = instance.Spec.CustomServiceConfig

	cms := []util.Template{
		// Custom ConfigMap
		{
			Name:         fmt.Sprintf("%s-config-data", instance.Name),
			Namespace:    instance.Namespace,
			Type:         util.TemplateTypeConfig,
			InstanceType: instance.Kind,
			CustomData:   customData,
			Labels:       cmLabels,
		},
	}

	err := configmap.EnsureConfigMaps(ctx, h, instance, cms, envVars)
	if err != nil {
		return nil
	}

	return nil
}

//
// createHashOfInputHashes - creates a hash of hashes which gets added to the resources which requires a restart
// if any of the input resources change, like configs, passwords, ...
//
func (r *CinderAPIReconciler) createHashOfInputHashes(
	ctx context.Context,
	instance *cinderv1beta1.CinderAPI,
	envVars map[string]env.Setter,
) (string, error) {
	mergedMapVars := env.MergeEnvs([]corev1.EnvVar{}, envVars)
	hash, err := util.ObjectHash(mergedMapVars)
	if err != nil {
		return hash, err
	}
	if hashMap, changed := util.SetHash(instance.Status.Hash, common.InputHashName, hash); changed {
		instance.Status.Hash = hashMap
		if err := r.Client.Status().Update(ctx, instance); err != nil {
			return hash, err
		}
		r.Log.Info(fmt.Sprintf("Input maps hash %s - %s", common.InputHashName, hash))
	}
	return hash, nil
}
