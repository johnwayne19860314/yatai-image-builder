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

package resources

import (
	"context"
	// nolint: gosec

	"os"
	"reflect"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/schemasv1"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
	"github.com/bentoml/yatai-image-builder/version"
)

// BentoRequestReconciler reconciles a BentoRequest object
type BentoRequestReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

//+kubebuilder:rbac:groups=resources.yatai.ai,resources=bentorequests,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=resources.yatai.ai,resources=bentorequests/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=resources.yatai.ai,resources=bentorequests/finalizers,verbs=update
//+kubebuilder:rbac:groups=resources.yatai.ai,resources=bentoes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=resources.yatai.ai,resources=bentoes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the BentoRequest object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *BentoRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	logs := log.FromContext(ctx)

	bentoRequest := &resourcesv1alpha1.BentoRequest{}

	err = r.Get(ctx, req.NamespacedName, bentoRequest)

	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			logs.Info("BentoRequest resource not found. Ignoring since object must be deleted")
			err = nil
			return
		}
		// Error reading the object - requeue the request.
		logs.Error(err, "Failed to get BentoRequest")
		return
	}

	if bentoRequest.Status.Conditions == nil || len(bentoRequest.Status.Conditions) == 0 {
		bentoRequest, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsSeeding,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting to reconcile BentoRequest",
			},
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting to reconcile BentoRequest",
			},
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExists,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Starting to reconcile BentoRequest",
			},
		)
		if err != nil {
			return
		}
	}

	logs = logs.WithValues("bentoRequest", bentoRequest.Name, "bentoRequestNamespace", bentoRequest.Namespace)

	defer func() {
		if err == nil {
			logs.Info("Reconcile success")
			return
		}
		logs.Error(err, "Failed to reconcile BentoRequest.")
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeWarning, "ReconcileError", "Failed to reconcile BentoRequest: %v", err)
		_, err_ := r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: err.Error(),
			},
		)
		if err_ != nil {
			logs.Error(err_, "Failed to update BentoRequest status")
			return
		}
	}()

	bentoAvailableCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable)
	if bentoAvailableCondition == nil || bentoAvailableCondition.Status != metav1.ConditionUnknown {
		bentoRequest, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: "Reconciling",
			},
		)
		if err != nil {
			return
		}
		result = ctrl.Result{
			Requeue: true,
		}
		return
	}

	separateModels := isSeparateModels(bentoRequest)

	modelsExists := false
	var modelsExistsResult ctrl.Result
	var modelsExistsErr error

	if separateModels {
		bentoRequest, modelsExists, modelsExistsResult, modelsExistsErr = r.ensureModelsExists(ctx, ensureModelsExistsOption{
			bentoRequest: bentoRequest,
			req:          req,
		})
	}

	bentoRequest, imageInfo, imageExists, imageExistsResult, err := r.ensureImageExists(ctx, ensureImageExistsOption{
		bentoRequest: bentoRequest,
		req:          req,
	})

	if err != nil {
		err = errors.Wrapf(err, "ensure image exists")
		return
	}

	if !imageExists {
		result = imageExistsResult
		return
	}

	if modelsExistsErr != nil {
		err = errors.Wrap(modelsExistsErr, "ensure model exists")
		return
	}

	if separateModels && !modelsExists {
		result = modelsExistsResult
		return
	}

	bentoCR := &resourcesv1alpha1.Bento{
		ObjectMeta: metav1.ObjectMeta{
			Name:      bentoRequest.Name,
			Namespace: bentoRequest.Namespace,
		},
		Spec: resourcesv1alpha1.BentoSpec{
			Tag:         bentoRequest.Spec.BentoTag,
			Image:       imageInfo.ImageName,
			ServiceName: bentoRequest.Spec.ServiceName,
			Context:     bentoRequest.Spec.Context,
			Runners:     bentoRequest.Spec.Runners,
			Models:      bentoRequest.Spec.Models,
		},
	}

	if separateModels {
		bentoCR.Annotations = map[string]string{
			commonconsts.KubeAnnotationYataiImageBuilderSeparateModels: commonconsts.KubeLabelValueTrue,
			commonconsts.KubeAnnotationAWSAccessKeySecretName:          bentoRequest.Annotations[commonconsts.KubeAnnotationAWSAccessKeySecretName],
		}
		if isAddNamespacePrefix() {
			bentoCR.Annotations[commonconsts.KubeAnnotationIsMultiTenancy] = commonconsts.KubeLabelValueTrue
		}
	}

	err = ctrl.SetControllerReference(bentoRequest, bentoCR, r.Scheme)
	if err != nil {
		err = errors.Wrap(err, "set controller reference")
		return
	}

	if imageInfo.DockerConfigJSONSecretName != "" {
		bentoCR.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
			{
				Name: imageInfo.DockerConfigJSONSecretName,
			},
		}
	}

	if bentoRequest.Spec.DownloadURL == "" {
		var bento *schemasv1.BentoFullSchema
		bento, err = r.getBento(ctx, bentoRequest)
		if err != nil {
			err = errors.Wrap(err, "get bento")
			return
		}
		bentoCR.Spec.Context = &resourcesv1alpha1.BentoContext{
			BentomlVersion: bento.Manifest.BentomlVersion,
		}
		bentoCR.Spec.Runners = make([]resourcesv1alpha1.BentoRunner, 0)
		for _, runner := range bento.Manifest.Runners {
			bentoCR.Spec.Runners = append(bentoCR.Spec.Runners, resourcesv1alpha1.BentoRunner{
				Name:         runner.Name,
				RunnableType: runner.RunnableType,
				ModelTags:    runner.Models,
			})
		}
	}

	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "BentoImageBuilder", "Creating Bento CR %s in namespace %s", bentoCR.Name, bentoCR.Namespace)
	err = r.Create(ctx, bentoCR)
	isAlreadyExists := k8serrors.IsAlreadyExists(err)
	if err != nil && !isAlreadyExists {
		err = errors.Wrap(err, "create Bento resource")
		return
	}
	if isAlreadyExists {
		oldBentoCR := &resourcesv1alpha1.Bento{}
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "BentoImageBuilder", "Updating Bento CR %s in namespace %s", bentoCR.Name, bentoCR.Namespace)
		err = r.Get(ctx, types.NamespacedName{Name: bentoCR.Name, Namespace: bentoCR.Namespace}, oldBentoCR)
		if err != nil {
			err = errors.Wrap(err, "get Bento resource")
			return
		}
		if !reflect.DeepEqual(oldBentoCR.Spec, bentoCR.Spec) {
			oldBentoCR.OwnerReferences = bentoCR.OwnerReferences
			oldBentoCR.Spec = bentoCR.Spec
			err = r.Update(ctx, oldBentoCR)
			if err != nil {
				err = errors.Wrap(err, "update Bento resource")
				return
			}
		}
	}

	bentoRequest, err = r.setStatusConditions(ctx, req,
		metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
			Status:  metav1.ConditionTrue,
			Reason:  "Reconciling",
			Message: "Bento is generated",
		},
	)
	if err != nil {
		return
	}

	return
}

// SetupWithManager sets up the controller with the Manager.
func (r *BentoRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	logs := log.Log.WithValues("func", "SetupWithManager")
	version.Print()

	if os.Getenv("DISABLE_YATAI_COMPONENT_REGISTRATION") != trueStr {
		go r.registerYataiComponent()
	} else {
		logs.Info("yatai component registration is disabled")
	}

	err := ctrl.NewControllerManagedBy(mgr).
		For(&resourcesv1alpha1.BentoRequest{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&resourcesv1alpha1.Bento{}).
		Owns(&batchv1.Job{}).
		Complete(r)
	return errors.Wrap(err, "failed to setup BentoRequest controller")
}
