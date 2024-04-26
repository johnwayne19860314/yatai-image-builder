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

	"fmt"
	"reflect"
	"time"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"strings"

	commonconsts "github.com/bentoml/yatai-common/consts"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
)

type ensureImageExistsOption struct {
	bentoRequest *resourcesv1alpha1.BentoRequest
	req          ctrl.Request
}

func (r *BentoRequestReconciler) ensureImageExists(ctx context.Context, opt ensureImageExistsOption) (bentoRequest *resourcesv1alpha1.BentoRequest, imageInfo ImageInfo, imageExists bool, result ctrl.Result, err error) {
	logs := log.FromContext(ctx)

	bentoRequest = opt.bentoRequest
	req := opt.req

	imageInfo, err = r.getImageInfo(ctx, GetImageInfoOption{
		BentoRequest: bentoRequest,
	})
	if err != nil {
		err = errors.Wrap(err, "get image info")
		return
	}

	imageExistsCheckedCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeImageExistsChecked)
	imageExistsCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeImageExists)
	if imageExistsCheckedCondition == nil || imageExistsCheckedCondition.Status != metav1.ConditionTrue || imageExistsCheckedCondition.Message != imageInfo.ImageName {
		imageExistsCheckedCondition = &metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExistsChecked,
			Status:  metav1.ConditionUnknown,
			Reason:  "Reconciling",
			Message: imageInfo.ImageName,
		}
		bentoAvailableCondition := &metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
			Status:  metav1.ConditionUnknown,
			Reason:  "Reconciling",
			Message: "Checking image exists",
		}
		bentoRequest, err = r.setStatusConditions(ctx, req, *imageExistsCheckedCondition, *bentoAvailableCondition)
		if err != nil {
			return
		}
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "CheckingImage", "Checking image exists: %s", imageInfo.ImageName)
		imageExists, err = checkImageExists(bentoRequest, imageInfo.DockerRegistry, imageInfo.InClusterImageName)
		if err != nil {
			err = errors.Wrapf(err, "check image %s exists", imageInfo.ImageName)
			return
		}

		err = r.Get(ctx, req.NamespacedName, bentoRequest)
		if err != nil {
			logs.Error(err, "Failed to re-fetch BentoRequest")
			return
		}

		if imageExists {
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "CheckingImage", "Image exists: %s", imageInfo.ImageName)
			imageExistsCheckedCondition = &metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExistsChecked,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: imageInfo.ImageName,
			}
			imageExistsCondition = &metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExists,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: imageInfo.ImageName,
			}
			bentoRequest, err = r.setStatusConditions(ctx, req, *imageExistsCondition, *imageExistsCheckedCondition)
			if err != nil {
				return
			}
		} else {
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "CheckingImage", "Image not exists: %s", imageInfo.ImageName)
			imageExistsCheckedCondition = &metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExistsChecked,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image not exists: %s", imageInfo.ImageName),
			}
			imageExistsCondition = &metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExists,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image %s is not exists", imageInfo.ImageName),
			}
			bentoRequest, err = r.setStatusConditions(ctx, req, *imageExistsCondition, *imageExistsCheckedCondition)
			if err != nil {
				return
			}
		}
	}

	var bentoRequestHashStr string
	bentoRequestHashStr, err = r.getHashStr(bentoRequest)
	if err != nil {
		err = errors.Wrapf(err, "get BentoRequest %s/%s hash string", bentoRequest.Namespace, bentoRequest.Name)
		return
	}

	imageExists = imageExistsCondition != nil && imageExistsCondition.Status == metav1.ConditionTrue && imageExistsCondition.Message == imageInfo.ImageName
	if imageExists {
		return
	}

	jobLabels := map[string]string{
		commonconsts.KubeLabelBentoRequest:        bentoRequest.Name,
		commonconsts.KubeLabelIsBentoImageBuilder: commonconsts.KubeLabelValueTrue,
	}

	if isSeparateModels(opt.bentoRequest) {
		jobLabels[KubeLabelYataiImageBuilderSeparateModels] = commonconsts.KubeLabelValueTrue
	} else {
		jobLabels[KubeLabelYataiImageBuilderSeparateModels] = commonconsts.KubeLabelValueFalse
	}

	jobs := &batchv1.JobList{}
	err = r.List(ctx, jobs, client.InNamespace(req.Namespace), client.MatchingLabels(jobLabels))
	if err != nil {
		err = errors.Wrap(err, "list jobs")
		return
	}

	reservedJobs := make([]*batchv1.Job, 0)
	for _, job_ := range jobs.Items {
		job_ := job_

		oldHash := job_.Annotations[KubeAnnotationBentoRequestHash]
		if oldHash != bentoRequestHashStr {
			logs.Info("Because hash changed, delete old job", "job", job_.Name, "oldHash", oldHash, "newHash", bentoRequestHashStr)
			// --cascade=foreground
			err = r.Delete(ctx, &job_, &client.DeleteOptions{
				PropagationPolicy: &[]metav1.DeletionPropagation{metav1.DeletePropagationForeground}[0],
			})
			if err != nil {
				err = errors.Wrapf(err, "delete job %s", job_.Name)
				return
			}
			return
		} else {
			reservedJobs = append(reservedJobs, &job_)
		}
	}

	var job *batchv1.Job
	if len(reservedJobs) > 0 {
		job = reservedJobs[0]
	}

	if len(reservedJobs) > 1 {
		for _, job_ := range reservedJobs[1:] {
			logs.Info("Because has more than one job, delete old job", "job", job_.Name)
			// --cascade=foreground
			err = r.Delete(ctx, job_, &client.DeleteOptions{
				PropagationPolicy: &[]metav1.DeletionPropagation{metav1.DeletePropagationForeground}[0],
			})
			if err != nil {
				err = errors.Wrapf(err, "delete job %s", job_.Name)
				return
			}
		}
	}

	if job == nil {
		job, err = r.generateImageBuilderJob(ctx, GenerateImageBuilderJobOption{
			ImageInfo:    imageInfo,
			BentoRequest: bentoRequest,
		})
		if err != nil {
			err = errors.Wrap(err, "generate image builder job")
			return
		}
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderJob", "Creating image builder job: %s", job.Name)
		err = r.Create(ctx, job)
		if err != nil {
			err = errors.Wrapf(err, "create image builder job %s", job.Name)
			return
		}
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderJob", "Created image builder job: %s", job.Name)
		return
	}

	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "CheckingImageBuilderJob", "Found image builder job: %s", job.Name)

	err = r.Get(ctx, req.NamespacedName, bentoRequest)
	if err != nil {
		logs.Error(err, "Failed to re-fetch BentoRequest")
		return
	}
	imageBuildingCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeImageBuilding)

	isJobFailed := false
	isJobRunning := true

	if job.Spec.Completions != nil {
		if job.Status.Succeeded != *job.Spec.Completions {
			if job.Status.Failed > 0 {
				for _, condition := range job.Status.Conditions {
					if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
						isJobFailed = true
						break
					}
				}
			}
			isJobRunning = !isJobFailed
		} else {
			isJobRunning = false
		}
	}

	if isJobRunning {
		conditions := make([]metav1.Condition, 0)
		if job.Status.Active > 0 {
			conditions = append(conditions, metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image building job %s is running", job.Name),
			})
		} else {
			conditions = append(conditions, metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
				Status:  metav1.ConditionUnknown,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image building job %s is waiting", job.Name),
			})
		}
		if bentoRequest.Spec.ImageBuildTimeout != nil {
			if imageBuildingCondition != nil && imageBuildingCondition.LastTransitionTime.Add(*bentoRequest.Spec.ImageBuildTimeout).Before(time.Now()) {
				conditions = append(conditions, metav1.Condition{
					Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
					Status:  metav1.ConditionFalse,
					Reason:  "Timeout",
					Message: fmt.Sprintf("Image building job %s is timeout", job.Name),
				})
				if _, err = r.setStatusConditions(ctx, req, conditions...); err != nil {
					return
				}
				err = errors.New("image build timeout")
				return
			}
		}

		if bentoRequest, err = r.setStatusConditions(ctx, req, conditions...); err != nil {
			return
		}

		if imageBuildingCondition != nil && imageBuildingCondition.Status != metav1.ConditionTrue && isJobRunning {
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "BentoImageBuilder", "Image is building now")
		}

		return
	}

	if isJobFailed {
		bentoRequest, err = r.setStatusConditions(ctx, req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image building job %s is failed.", job.Name),
			},
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Image building job %s is failed.", job.Name),
			},
		)
		if err != nil {
			return
		}
		err = errors.Errorf("Image built failed, image builder job %s is failed", job.Name)
		return
	}

	bentoRequest, err = r.setStatusConditions(ctx, req,
		metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeImageBuilding,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: fmt.Sprintf("Image building job %s is successed.", job.Name),
		},
		metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeImageExists,
			Status:  metav1.ConditionTrue,
			Reason:  "Reconciling",
			Message: imageInfo.ImageName,
		},
	)
	if err != nil {
		return
	}

	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "BentoImageBuilder", "Image has been built successfully")

	imageExists = true

	return
}

type ensureModelsExistsOption struct {
	bentoRequest *resourcesv1alpha1.BentoRequest
	req          ctrl.Request
}

func (r *BentoRequestReconciler) ensureModelsExists(ctx context.Context, opt ensureModelsExistsOption) (bentoRequest *resourcesv1alpha1.BentoRequest, modelsExists bool, result ctrl.Result, err error) {
	bentoRequest = opt.bentoRequest
	modelTags := make([]string, 0)
	for _, model := range bentoRequest.Spec.Models {
		modelTags = append(modelTags, model.Tag)
	}

	modelsExistsCondition := meta.FindStatusCondition(bentoRequest.Status.Conditions, resourcesv1alpha1.BentoRequestConditionTypeModelsExists)
	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "SeparateModels", "Separate models are enabled")
	if modelsExistsCondition == nil || modelsExistsCondition.Status == metav1.ConditionUnknown {
		r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "ModelsExists", "Models are not ready")
		modelsExistsCondition = &metav1.Condition{
			Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsExists,
			Status:  metav1.ConditionFalse,
			Reason:  "Reconciling",
			Message: "Models are not ready",
		}
		bentoRequest, err = r.setStatusConditions(ctx, opt.req, *modelsExistsCondition)
		if err != nil {
			return
		}
	}

	modelsExists = modelsExistsCondition != nil && modelsExistsCondition.Status == metav1.ConditionTrue && modelsExistsCondition.Message == fmt.Sprintf("%s:%s", getJuiceFSStorageClassName(), strings.Join(modelTags, ", "))
	if modelsExists {
		return
	}

	modelsMap := make(map[string]*resourcesv1alpha1.BentoModel)
	for _, model := range bentoRequest.Spec.Models {
		model := model
		modelsMap[model.Tag] = &model
	}

	jobLabels := map[string]string{
		commonconsts.KubeLabelBentoRequest:  bentoRequest.Name,
		commonconsts.KubeLabelIsModelSeeder: "true",
	}

	jobs := &batchv1.JobList{}
	err = r.List(ctx, jobs, client.InNamespace(bentoRequest.Namespace), client.MatchingLabels(jobLabels))
	if err != nil {
		err = errors.Wrap(err, "list jobs")
		return
	}

	var bentoRequestHashStr string
	bentoRequestHashStr, err = r.getHashStr(bentoRequest)
	if err != nil {
		err = errors.Wrapf(err, "get BentoRequest %s/%s hash string", bentoRequest.Namespace, bentoRequest.Name)
		return
	}

	existingJobModelTags := make(map[string]struct{})
	for _, job_ := range jobs.Items {
		job_ := job_

		oldHash := job_.Annotations[KubeAnnotationBentoRequestHash]
		if oldHash != bentoRequestHashStr {
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "DeleteJob", "Because hash changed, delete old job %s, oldHash: %s, newHash: %s", job_.Name, oldHash, bentoRequestHashStr)
			// --cascade=foreground
			err = r.Delete(ctx, &job_, &client.DeleteOptions{
				PropagationPolicy: &[]metav1.DeletionPropagation{metav1.DeletePropagationForeground}[0],
			})
			if err != nil {
				err = errors.Wrapf(err, "delete job %s", job_.Name)
				return
			}
			continue
		}

		modelTag := fmt.Sprintf("%s:%s", job_.Labels[commonconsts.KubeLabelYataiModelRepository], job_.Labels[commonconsts.KubeLabelYataiModel])
		_, ok := modelsMap[modelTag]

		if !ok {
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "DeleteJob", "Due to the nonexistence of the model %s, job %s has been deleted.", modelTag, job_.Name)
			// --cascade=foreground
			err = r.Delete(ctx, &job_, &client.DeleteOptions{
				PropagationPolicy: &[]metav1.DeletionPropagation{metav1.DeletePropagationForeground}[0],
			})
			if err != nil {
				err = errors.Wrapf(err, "delete job %s", job_.Name)
				return
			}
		} else {
			existingJobModelTags[modelTag] = struct{}{}
		}
	}

	for _, model := range bentoRequest.Spec.Models {
		if _, ok := existingJobModelTags[model.Tag]; ok {
			continue
		}
		model := model
		pvc := &corev1.PersistentVolumeClaim{}
		pvcName := r.getModelPVCName(bentoRequest, &model)
		err = r.Get(ctx, client.ObjectKey{
			Namespace: bentoRequest.Namespace,
			Name:      pvcName,
		}, pvc)
		isPVCNotFound := k8serrors.IsNotFound(err)
		if err != nil && !isPVCNotFound {
			err = errors.Wrapf(err, "get PVC %s/%s", bentoRequest.Namespace, pvcName)
			return
		}
		if isPVCNotFound {
			pvc = r.generateModelPVC(GenerateModelPVCOption{
				BentoRequest: bentoRequest,
				Model:        &model,
			})
			err = r.Create(ctx, pvc)
			isPVCAlreadyExists := k8serrors.IsAlreadyExists(err)
			if err != nil && !isPVCAlreadyExists {
				err = errors.Wrapf(err, "create model %s/%s pvc", bentoRequest.Namespace, model.Tag)
				return
			}
		}
		var job *batchv1.Job
		job, err = r.generateModelSeederJob(ctx, GenerateModelSeederJobOption{
			BentoRequest: bentoRequest,
			Model:        &model,
		})
		if err != nil {
			err = errors.Wrap(err, "generate model seeder job")
			return
		}
		oldJob := &batchv1.Job{}
		err = r.Get(ctx, client.ObjectKeyFromObject(job), oldJob)
		oldJobIsNotFound := k8serrors.IsNotFound(err)
		if err != nil && !oldJobIsNotFound {
			err = errors.Wrap(err, "get job")
			return
		}
		if oldJobIsNotFound {
			err = r.Create(ctx, job)
			if err != nil {
				err = errors.Wrap(err, "create job")
				return
			}
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "CreateJob", "Job %s has been created.", job.Name)
		} else if !reflect.DeepEqual(job.Labels, oldJob.Labels) || !reflect.DeepEqual(job.Annotations, oldJob.Annotations) {
			job.Labels = oldJob.Labels
			job.Annotations = oldJob.Annotations
			err = r.Update(ctx, job)
			if err != nil {
				err = errors.Wrap(err, "update job")
				return
			}
			r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "UpdateJob", "Job %s has been updated.", job.Name)
		}
	}

	jobs = &batchv1.JobList{}
	err = r.List(ctx, jobs, client.InNamespace(bentoRequest.Namespace), client.MatchingLabels(jobLabels))
	if err != nil {
		err = errors.Wrap(err, "list jobs")
		return
	}

	succeedModelTags := make(map[string]struct{})
	failedJobNames := make([]string, 0)
	notReadyJobNames := make([]string, 0)
	for _, job_ := range jobs.Items {
		if job_.Spec.Completions != nil && job_.Status.Succeeded == *job_.Spec.Completions {
			modelTag := fmt.Sprintf("%s:%s", job_.Labels[commonconsts.KubeLabelYataiModelRepository], job_.Labels[commonconsts.KubeLabelYataiModel])
			succeedModelTags[modelTag] = struct{}{}
			continue
		}
		if job_.Status.Failed > 0 {
			for _, condition := range job_.Status.Conditions {
				if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
					failedJobNames = append(failedJobNames, job_.Name)
					continue
				}
			}
		}
		notReadyJobNames = append(notReadyJobNames, job_.Name)
	}

	if len(failedJobNames) > 0 {
		msg := fmt.Sprintf("Model seeder jobs failed: %s", strings.Join(failedJobNames, ", "))
		r.Recorder.Event(bentoRequest, corev1.EventTypeNormal, "ModelsExists", msg)
		bentoRequest, err = r.setStatusConditions(ctx, opt.req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsExists,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: msg,
			},
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeBentoAvailable,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: msg,
			},
		)
		if err != nil {
			return
		}
		err = errors.New(msg)
		return
	}

	modelsExists = true

	for _, model := range bentoRequest.Spec.Models {
		if _, ok := succeedModelTags[model.Tag]; !ok {
			modelsExists = false
			break
		}
	}

	if modelsExists {
		bentoRequest, err = r.setStatusConditions(ctx, opt.req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsExists,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("%s:%s", getJuiceFSStorageClassName(), strings.Join(modelTags, ", ")),
			},
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsSeeding,
				Status:  metav1.ConditionFalse,
				Reason:  "Reconciling",
				Message: "All models have been seeded.",
			},
		)
		if err != nil {
			return
		}
	} else {
		bentoRequest, err = r.setStatusConditions(ctx, opt.req,
			metav1.Condition{
				Type:    resourcesv1alpha1.BentoRequestConditionTypeModelsSeeding,
				Status:  metav1.ConditionTrue,
				Reason:  "Reconciling",
				Message: fmt.Sprintf("Model seeder jobs are not ready: %s.", strings.Join(notReadyJobNames, ", ")),
			},
		)
		if err != nil {
			return
		}
	}
	return
}

func (r *BentoRequestReconciler) setStatusConditions(ctx context.Context, req ctrl.Request, conditions ...metav1.Condition) (bentoRequest *resourcesv1alpha1.BentoRequest, err error) {
	bentoRequest = &resourcesv1alpha1.BentoRequest{}
	/*
		Please don't blame me when you see this kind of code,
		this is to avoid "the object has been modified; please apply your changes to the latest version and try again" when updating CR status,
		don't doubt that almost all CRD operators (e.g. cert-manager) can't avoid this stupid error and can only try to avoid this by this stupid way.
	*/
	for i := 0; i < 3; i++ {
		if err = r.Get(ctx, req.NamespacedName, bentoRequest); err != nil {
			err = errors.Wrap(err, "Failed to re-fetch BentoRequest")
			return
		}
		for _, condition := range conditions {
			meta.SetStatusCondition(&bentoRequest.Status.Conditions, condition)
		}
		if err = r.Status().Update(ctx, bentoRequest); err != nil {
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}
	if err != nil {
		err = errors.Wrap(err, "Failed to update BentoRequest status")
		return
	}
	if err = r.Get(ctx, req.NamespacedName, bentoRequest); err != nil {
		err = errors.Wrap(err, "Failed to re-fetch BentoRequest")
		return
	}
	return
}
