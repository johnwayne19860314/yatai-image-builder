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
	"os"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"bytes"
	"text/template"

	"github.com/huandu/xstrings"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/resource"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
	yataiclient "github.com/bentoml/yatai-image-builder/yatai-client"
)

func (r *BentoRequestReconciler) generateModelPVC(opt GenerateModelPVCOption) (pvc *corev1.PersistentVolumeClaim) {
	storageSize := resource.MustParse("100Gi")
	if opt.Model.Size != nil {
		storageSize = *opt.Model.Size
		minStorageSize := resource.MustParse("1Gi")
		if storageSize.Value() < minStorageSize.Value() {
			storageSize = minStorageSize
		}
		storageSize.Set(storageSize.Value() * 2)
	}
	path := r.getJuiceFSModelPath(opt.BentoRequest, opt.Model)
	pvcName := r.getModelPVCName(opt.BentoRequest, opt.Model)
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: opt.BentoRequest.Namespace,
			Annotations: map[string]string{
				"path": path,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
			StorageClassName: pointer.StringPtr(getJuiceFSStorageClassName()),
		},
	}
	return
}

func (r *BentoRequestReconciler) generateModelSeederJob(ctx context.Context, opt GenerateModelSeederJobOption) (job *batchv1.Job, err error) {
	// nolint: gosimple
	podTemplateSpec, err := r.generateModelSeederPodTemplateSpec(ctx, GenerateModelSeederPodTemplateSpecOption{
		BentoRequest: opt.BentoRequest,
		Model:        opt.Model,
	})
	if err != nil {
		err = errors.Wrap(err, "generate model seeder pod template spec")
		return
	}
	kubeAnnotations := make(map[string]string)
	hashStr, err := r.getHashStr(opt.BentoRequest)
	if err != nil {
		err = errors.Wrap(err, "failed to get hash string")
		return
	}
	kubeAnnotations[KubeAnnotationBentoRequestHash] = hashStr
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        r.getModelSeederJobName(),
			Namespace:   opt.BentoRequest.Namespace,
			Labels:      r.getModelSeederJobLabels(opt.BentoRequest, opt.Model),
			Annotations: kubeAnnotations,
		},
		Spec: batchv1.JobSpec{
			Completions: pointer.Int32Ptr(1),
			Parallelism: pointer.Int32Ptr(1),
			PodFailurePolicy: &batchv1.PodFailurePolicy{
				Rules: []batchv1.PodFailurePolicyRule{
					{
						Action: batchv1.PodFailurePolicyActionFailJob,
						OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
							ContainerName: pointer.StringPtr(ModelSeederContainerName),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:        []int32{ModelSeederJobFailedExitCode},
						},
					},
				},
			},
			Template: *podTemplateSpec,
		},
	}
	err = ctrl.SetControllerReference(opt.BentoRequest, job, r.Scheme)
	if err != nil {
		err = errors.Wrapf(err, "set controller reference for job %s", job.Name)
		return
	}
	return
}

func (r *BentoRequestReconciler) generateModelSeederPodTemplateSpec(ctx context.Context, opt GenerateModelSeederPodTemplateSpecOption) (pod *corev1.PodTemplateSpec, err error) {
	modelRepositoryName, _, modelVersion := xstrings.Partition(opt.BentoRequest.Spec.BentoTag, ":")
	kubeLabels := r.getModelSeederPodLabels(opt.BentoRequest, opt.Model)

	volumes := make([]corev1.Volume, 0)

	volumeMounts := make([]corev1.VolumeMount, 0)

	yataiAPITokenSecretName := ""

	// nolint: gosec
	awsAccessKeySecretName := opt.BentoRequest.Annotations[commonconsts.KubeAnnotationAWSAccessKeySecretName]
	if awsAccessKeySecretName == "" {
		awsAccessKeyID := os.Getenv(commonconsts.EnvAWSAccessKeyID)
		awsSecretAccessKey := os.Getenv(commonconsts.EnvAWSSecretAccessKey)
		if awsAccessKeyID != "" && awsSecretAccessKey != "" {
			// nolint: gosec
			awsAccessKeySecretName = YataiImageBuilderAWSAccessKeySecretName
			stringData := map[string]string{
				commonconsts.EnvAWSAccessKeyID:     awsAccessKeyID,
				commonconsts.EnvAWSSecretAccessKey: awsSecretAccessKey,
			}
			awsAccessKeySecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      awsAccessKeySecretName,
					Namespace: opt.BentoRequest.Namespace,
				},
				StringData: stringData,
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Creating or updating secret %s in namespace %s", awsAccessKeySecretName, opt.BentoRequest.Namespace)
			_, err = controllerutil.CreateOrUpdate(ctx, r.Client, awsAccessKeySecret, func() error {
				awsAccessKeySecret.StringData = stringData
				return nil
			})
			if err != nil {
				err = errors.Wrapf(err, "failed to create or update secret %s", awsAccessKeySecretName)
				return
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is created or updated in namespace %s", awsAccessKeySecretName, opt.BentoRequest.Namespace)
		}
	}

	internalImages := commonconfig.GetInternalImages()
	logrus.Infof("Model seeder is using the images %v", *internalImages)

	downloaderContainerResources := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("3000Mi"),
		},
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("1000Mi"),
		},
	}

	downloaderContainerEnvFrom := opt.BentoRequest.Spec.DownloaderContainerEnvFrom

	if yataiAPITokenSecretName != "" {
		downloaderContainerEnvFrom = append(downloaderContainerEnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: yataiAPITokenSecretName,
				},
			},
		})
	}

	if awsAccessKeySecretName != "" {
		downloaderContainerEnvFrom = append(downloaderContainerEnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: awsAccessKeySecretName,
				},
			},
		})
	}

	containers := make([]corev1.Container, 0)

	model := opt.Model

	modelDownloadURL := model.DownloadURL
	modelDownloadHeader := ""
	if modelDownloadURL == "" {
		var yataiClient_ **yataiclient.YataiClient
		var yataiConf_ **commonconfig.YataiConfig

		yataiClient_, yataiConf_, err = r.getYataiClient(ctx)
		if err != nil {
			err = errors.Wrap(err, "get yatai client")
			return
		}

		if yataiClient_ == nil || yataiConf_ == nil {
			err = errors.New("can't get yatai client, please check yatai configuration")
			return
		}

		yataiClient := *yataiClient_
		yataiConf := *yataiConf_

		var model_ *schemasv1.ModelFullSchema
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting model %s from yatai service", model.Tag)
		model_, err = yataiClient.GetModel(ctx, modelRepositoryName, modelVersion)
		if err != nil {
			err = errors.Wrap(err, "get model")
			return
		}
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Model %s is got from yatai service", model.Tag)

		if model_.TransmissionStrategy != nil && *model_.TransmissionStrategy == modelschemas.TransmissionStrategyPresignedURL {
			var model0 *schemasv1.ModelSchema
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting presigned url for model %s from yatai service", model.Tag)
			model0, err = yataiClient.PresignModelDownloadURL(ctx, modelRepositoryName, modelVersion)
			if err != nil {
				err = errors.Wrap(err, "presign model download url")
				return
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Presigned url for model %s is got from yatai service", model.Tag)
			modelDownloadURL = model0.PresignedDownloadUrl
		} else {
			modelDownloadURL = fmt.Sprintf("%s/api/v1/model_repositories/%s/models/%s/download", yataiConf.Endpoint, modelRepositoryName, modelVersion)
			modelDownloadHeader = fmt.Sprintf("%s: %s:%s:$%s", commonconsts.YataiApiTokenHeaderName, commonconsts.YataiImageBuilderComponentName, yataiConf.ClusterName, commonconsts.EnvYataiApiToken)
		}
	}

	modelDirPath := "/juicefs-workspace"
	var modelSeedCommandOutput bytes.Buffer
	err = template.Must(template.New("script").Parse(`
set -e

if [ -f "{{.ModelDirPath}}/.exists" ]; then
	echo "Model {{.ModelDirPath}} already exists, skip downloading"
	exit 0
fi

mkdir -p {{.ModelDirPath}}
url="{{.ModelDownloadURL}}"
echo "Downloading model {{.ModelRepositoryName}}:{{.ModelVersion}} tar file from ${url} to /tmp/downloaded.tar..."
if [[ ${url} == s3://* ]]; then
	echo "Downloading from s3..."
	aws s3 cp ${url} /tmp/downloaded.tar
else
	curl --fail -L -H "{{.ModelDownloadHeader}}" ${url} --output /tmp/downloaded.tar --progress-bar
fi
cd {{.ModelDirPath}}
echo "Extracting model tar file..."
tar -xvf /tmp/downloaded.tar
echo "Creating {{.ModelDirPath}}/.exists file..."
touch {{.ModelDirPath}}/.exists
echo "Removing model tar file..."
rm /tmp/downloaded.tar
echo "Done"
`)).Execute(&modelSeedCommandOutput, map[string]interface{}{
		"ModelDirPath":        modelDirPath,
		"ModelDownloadURL":    modelDownloadURL,
		"ModelDownloadHeader": modelDownloadHeader,
		"ModelRepositoryName": modelRepositoryName,
		"ModelVersion":        modelVersion,
	})
	if err != nil {
		err = errors.Wrap(err, "failed to generate download command")
		return
	}
	modelSeedCommand := modelSeedCommandOutput.String()
	pvcName := r.getModelPVCName(opt.BentoRequest, model)
	volumes = append(volumes, corev1.Volume{
		Name: pvcName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	})
	containers = append(containers, corev1.Container{
		Name:  ModelSeederContainerName,
		Image: internalImages.BentoDownloader,
		Command: []string{
			"bash",
			"-c",
			modelSeedCommand,
		},
		VolumeMounts: append(volumeMounts, corev1.VolumeMount{
			Name:      pvcName,
			MountPath: "/juicefs-workspace",
		}),
		Resources: downloaderContainerResources,
		EnvFrom:   downloaderContainerEnvFrom,
		Env: []corev1.EnvVar{
			{
				Name:  "AWS_EC2_METADATA_DISABLED",
				Value: "true",
			},
		},
	})

	kubeAnnotations := make(map[string]string)

	pod = &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      kubeLabels,
			Annotations: kubeAnnotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes:       volumes,
			Containers:    containers,
		},
	}

	var globalExtraPodSpec *resourcesv1alpha1.ExtraPodSpec

	configNamespace, err := commonconfig.GetYataiImageBuilderNamespace(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, secret)
		return secret, errors.Wrap(err, "get secret")
	})
	if err != nil {
		err = errors.Wrap(err, "failed to get Yatai image builder namespace")
		return
	}

	configCmName := "yatai-image-builder-config"
	r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateModelSeederPod", "Getting configmap %s from namespace %s", configCmName, configNamespace)
	configCm := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: configCmName, Namespace: configNamespace}, configCm)
	configCmIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !configCmIsNotFound {
		err = errors.Wrap(err, "failed to get configmap")
		return
	}
	err = nil

	if !configCmIsNotFound {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateModelSeederPod", "Configmap %s is got from namespace %s", configCmName, configNamespace)

		globalExtraPodSpec = &resourcesv1alpha1.ExtraPodSpec{}

		if val, ok := configCm.Data["extra_pod_spec"]; ok {
			err = yaml.Unmarshal([]byte(val), globalExtraPodSpec)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal extra_pod_spec, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}
	} else {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateModelSeederPod", "Configmap %s is not found in namespace %s", configCmName, configNamespace)
	}

	if globalExtraPodSpec != nil {
		pod.Spec.PriorityClassName = globalExtraPodSpec.PriorityClassName
		pod.Spec.SchedulerName = globalExtraPodSpec.SchedulerName
		pod.Spec.NodeSelector = globalExtraPodSpec.NodeSelector
		pod.Spec.Affinity = globalExtraPodSpec.Affinity
		pod.Spec.Tolerations = globalExtraPodSpec.Tolerations
		pod.Spec.TopologySpreadConstraints = globalExtraPodSpec.TopologySpreadConstraints
		pod.Spec.ServiceAccountName = globalExtraPodSpec.ServiceAccountName
	}

	return
}
