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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"bytes"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/huandu/xstrings"
	"github.com/iancoleman/strcase"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/resource"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
	yataiclient "github.com/bentoml/yatai-image-builder/yatai-client"
)

func (r *BentoRequestReconciler) generateImageBuilderJob(ctx context.Context, opt GenerateImageBuilderJobOption) (job *batchv1.Job, err error) {
	// nolint: gosimple
	podTemplateSpec, err := r.generateImageBuilderPodTemplateSpec(ctx, GenerateImageBuilderPodTemplateSpecOption{
		ImageInfo:    opt.ImageInfo,
		BentoRequest: opt.BentoRequest,
	})
	if err != nil {
		err = errors.Wrap(err, "generate image builder pod template spec")
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
			Name:        r.getImageBuilderJobName(),
			Namespace:   opt.BentoRequest.Namespace,
			Labels:      r.getImageBuilderJobLabels(opt.BentoRequest),
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
							ContainerName: pointer.StringPtr(BuilderContainerName),
							Operator:      batchv1.PodFailurePolicyOnExitCodesOpIn,
							Values:        []int32{BuilderJobFailedExitCode},
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

func (r *BentoRequestReconciler) generateImageBuilderPodTemplateSpec(ctx context.Context, opt GenerateImageBuilderPodTemplateSpecOption) (pod *corev1.PodTemplateSpec, err error) {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(opt.BentoRequest.Spec.BentoTag, ":")
	kubeLabels := r.getImageBuilderPodLabels(opt.BentoRequest)

	inClusterImageName := opt.ImageInfo.InClusterImageName

	dockerConfigJSONSecretName := opt.ImageInfo.DockerConfigJSONSecretName

	dockerRegistryInsecure := opt.ImageInfo.DockerRegistryInsecure

	volumes := []corev1.Volume{
		{
			Name: "yatai",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "yatai",
			MountPath: "/yatai",
		},
		{
			Name:      "workspace",
			MountPath: "/workspace",
		},
	}

	if dockerConfigJSONSecretName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: dockerConfigJSONSecretName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: dockerConfigJSONSecretName,
					Items: []corev1.KeyToPath{
						{
							Key:  ".dockerconfigjson",
							Path: "config.json",
						},
					},
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      dockerConfigJSONSecretName,
			MountPath: "/kaniko/.docker/",
		})
	}

	var bento *schemasv1.BentoFullSchema
	yataiAPITokenSecretName := ""
	bentoDownloadURL := opt.BentoRequest.Spec.DownloadURL
	bentoDownloadHeader := ""

	if bentoDownloadURL == "" {
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

		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting bento %s from yatai service", opt.BentoRequest.Spec.BentoTag)
		bento, err = yataiClient.GetBento(ctx, bentoRepositoryName, bentoVersion)
		if err != nil {
			err = errors.Wrap(err, "get bento")
			return
		}
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Got bento %s from yatai service", opt.BentoRequest.Spec.BentoTag)

		if bento.TransmissionStrategy != nil && *bento.TransmissionStrategy == modelschemas.TransmissionStrategyPresignedURL {
			var bento_ *schemasv1.BentoSchema
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting presigned url for bento %s from yatai service", opt.BentoRequest.Spec.BentoTag)
			bento_, err = yataiClient.PresignBentoDownloadURL(ctx, bentoRepositoryName, bentoVersion)
			if err != nil {
				err = errors.Wrap(err, "presign bento download url")
				return
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Got presigned url for bento %s from yatai service", opt.BentoRequest.Spec.BentoTag)
			bentoDownloadURL = bento_.PresignedDownloadUrl
		} else {
			bentoDownloadURL = fmt.Sprintf("%s/api/v1/bento_repositories/%s/bentos/%s/download", yataiConf.Endpoint, bentoRepositoryName, bentoVersion)
			bentoDownloadHeader = fmt.Sprintf("%s: %s:%s:$%s", commonconsts.YataiApiTokenHeaderName, commonconsts.YataiImageBuilderComponentName, yataiConf.ClusterName, commonconsts.EnvYataiApiToken)
		}

		// nolint: gosec
		yataiAPITokenSecretName = "yatai-api-token"

		yataiAPITokenSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      yataiAPITokenSecretName,
				Namespace: opt.BentoRequest.Namespace,
			},
			StringData: map[string]string{
				commonconsts.EnvYataiApiToken: yataiConf.ApiToken,
			},
		}

		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting secret %s in namespace %s", yataiAPITokenSecretName, opt.BentoRequest.Namespace)
		_yataiAPITokenSecret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Namespace: opt.BentoRequest.Namespace, Name: yataiAPITokenSecretName}, _yataiAPITokenSecret)
		isNotFound := k8serrors.IsNotFound(err)
		if err != nil && !isNotFound {
			err = errors.Wrapf(err, "failed to get secret %s", yataiAPITokenSecretName)
			return
		}

		if isNotFound {
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is not found, so creating it in namespace %s", yataiAPITokenSecretName, opt.BentoRequest.Namespace)
			err = r.Create(ctx, yataiAPITokenSecret)
			isExists := k8serrors.IsAlreadyExists(err)
			if err != nil && !isExists {
				err = errors.Wrapf(err, "failed to create secret %s", yataiAPITokenSecretName)
				return
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is created in namespace %s", yataiAPITokenSecretName, opt.BentoRequest.Namespace)
		} else {
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is found in namespace %s, so updating it", yataiAPITokenSecretName, opt.BentoRequest.Namespace)
			err = r.Update(ctx, yataiAPITokenSecret)
			if err != nil {
				err = errors.Wrapf(err, "failed to update secret %s", yataiAPITokenSecretName)
				return
			}
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is updated in namespace %s", yataiAPITokenSecretName, opt.BentoRequest.Namespace)
		}
	}

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
	logrus.Infof("Image builder is using the images %v", *internalImages)

	buildEngine := getBentoImageBuildEngine()

	privileged := buildEngine != BentoImageBuildEngineBuildkitRootless

	bentoDownloadCommandTemplate, err := template.New("downloadCommand").Parse(`
set -e

mkdir -p /workspace/buildcontext
url="{{.BentoDownloadURL}}"
echo "Downloading bento {{.BentoRepositoryName}}:{{.BentoVersion}} tar file from ${url} to /tmp/downloaded.tar..."
if [[ ${url} == s3://* ]]; then
	echo "Downloading from s3..."
	aws s3 cp ${url} /tmp/downloaded.tar
else
	curl --fail -L -H "{{.BentoDownloadHeader}}" ${url} --output /tmp/downloaded.tar --progress-bar
fi
cd /workspace/buildcontext
echo "Extracting bento tar file..."
tar -xvf /tmp/downloaded.tar
echo "Removing bento tar file..."
rm /tmp/downloaded.tar
{{if not .Privileged}}
echo "Changing directory permission..."
chown -R 1000:1000 /workspace
{{end}}
echo "Done"
	`)

	if err != nil {
		err = errors.Wrap(err, "failed to parse download command template")
		return
	}

	var bentoDownloadCommandBuffer bytes.Buffer

	err = bentoDownloadCommandTemplate.Execute(&bentoDownloadCommandBuffer, map[string]interface{}{
		"BentoDownloadURL":    bentoDownloadURL,
		"BentoDownloadHeader": bentoDownloadHeader,
		"BentoRepositoryName": bentoRepositoryName,
		"BentoVersion":        bentoVersion,
		"Privileged":          privileged,
	})
	if err != nil {
		err = errors.Wrap(err, "failed to execute download command template")
		return
	}

	bentoDownloadCommand := bentoDownloadCommandBuffer.String()

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

	initContainers := []corev1.Container{
		{
			Name:  "bento-downloader",
			Image: internalImages.BentoDownloader,
			Command: []string{
				"bash",
				"-c",
				bentoDownloadCommand,
			},
			VolumeMounts: volumeMounts,
			Resources:    downloaderContainerResources,
			EnvFrom:      downloaderContainerEnvFrom,
			Env: []corev1.EnvVar{
				{
					Name:  "AWS_EC2_METADATA_DISABLED",
					Value: "true",
				},
			},
		},
	}

	containers := make([]corev1.Container, 0)

	separateModels := isSeparateModels(opt.BentoRequest)

	models := opt.BentoRequest.Spec.Models
	modelsSeen := map[string]struct{}{}
	for _, model := range models {
		modelsSeen[model.Tag] = struct{}{}
	}

	if bento != nil {
		for _, modelTag := range bento.Manifest.Models {
			if _, ok := modelsSeen[modelTag]; !ok {
				models = append(models, resourcesv1alpha1.BentoModel{
					Tag: modelTag,
				})
			}
		}
	}

	for idx, model := range models {
		if separateModels {
			continue
		}
		modelRepositoryName, _, modelVersion := xstrings.Partition(model.Tag, ":")
		modelDownloadURL := model.DownloadURL
		modelDownloadHeader := ""
		if modelDownloadURL == "" {
			if bento == nil {
				continue
			}

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
		modelRepositoryDirPath := fmt.Sprintf("/workspace/buildcontext/models/%s", modelRepositoryName)
		modelDirPath := filepath.Join(modelRepositoryDirPath, modelVersion)
		var modelDownloadCommandOutput bytes.Buffer
		err = template.Must(template.New("script").Parse(`
set -e

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
echo -n '{{.ModelVersion}}' > {{.ModelRepositoryDirPath}}/latest
echo "Removing model tar file..."
rm /tmp/downloaded.tar
{{if not .Privileged}}
echo "Changing directory permission..."
chown -R 1000:1000 /workspace
{{end}}
echo "Done"
`)).Execute(&modelDownloadCommandOutput, map[string]interface{}{
			"ModelDirPath":           modelDirPath,
			"ModelDownloadURL":       modelDownloadURL,
			"ModelDownloadHeader":    modelDownloadHeader,
			"ModelRepositoryDirPath": modelRepositoryDirPath,
			"ModelRepositoryName":    modelRepositoryName,
			"ModelVersion":           modelVersion,
			"Privileged":             privileged,
		})
		if err != nil {
			err = errors.Wrap(err, "failed to generate download command")
			return
		}
		modelDownloadCommand := modelDownloadCommandOutput.String()
		initContainers = append(initContainers, corev1.Container{
			Name:  fmt.Sprintf("model-downloader-%d", idx),
			Image: internalImages.BentoDownloader,
			Command: []string{
				"bash",
				"-c",
				modelDownloadCommand,
			},
			VolumeMounts: volumeMounts,
			Resources:    downloaderContainerResources,
			EnvFrom:      downloaderContainerEnvFrom,
			Env: []corev1.EnvVar{
				{
					Name:  "AWS_EC2_METADATA_DISABLED",
					Value: "true",
				},
			},
		})
	}

	var globalExtraPodMetadata *resourcesv1alpha1.ExtraPodMetadata
	var globalExtraPodSpec *resourcesv1alpha1.ExtraPodSpec
	var globalExtraContainerEnv []corev1.EnvVar
	var globalDefaultImageBuilderContainerResources *corev1.ResourceRequirements
	var buildArgs []string
	var builderArgs []string

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
	r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting configmap %s from namespace %s", configCmName, configNamespace)
	configCm := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: configCmName, Namespace: configNamespace}, configCm)
	configCmIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !configCmIsNotFound {
		err = errors.Wrap(err, "failed to get configmap")
		return
	}
	err = nil // nolint: ineffassign

	if !configCmIsNotFound {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Configmap %s is got from namespace %s", configCmName, configNamespace)

		globalExtraPodMetadata = &resourcesv1alpha1.ExtraPodMetadata{}

		if val, ok := configCm.Data["extra_pod_metadata"]; ok {
			err = yaml.Unmarshal([]byte(val), globalExtraPodMetadata)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal extra_pod_metadata, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}

		globalExtraPodSpec = &resourcesv1alpha1.ExtraPodSpec{}

		if val, ok := configCm.Data["extra_pod_spec"]; ok {
			err = yaml.Unmarshal([]byte(val), globalExtraPodSpec)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal extra_pod_spec, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}

		globalExtraContainerEnv = []corev1.EnvVar{}

		if val, ok := configCm.Data["extra_container_env"]; ok {
			err = yaml.Unmarshal([]byte(val), &globalExtraContainerEnv)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal extra_container_env, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}

		if val, ok := configCm.Data["default_image_builder_container_resources"]; ok {
			globalDefaultImageBuilderContainerResources = &corev1.ResourceRequirements{}
			err = yaml.Unmarshal([]byte(val), globalDefaultImageBuilderContainerResources)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal default_image_builder_container_resources, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}

		buildArgs = []string{}

		if val, ok := configCm.Data["build_args"]; ok {
			err = yaml.Unmarshal([]byte(val), &buildArgs)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal build_args, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}

		builderArgs = []string{}
		if val, ok := configCm.Data["builder_args"]; ok {
			err = yaml.Unmarshal([]byte(val), &builderArgs)
			if err != nil {
				err = errors.Wrapf(err, "failed to yaml unmarshal builder_args, please check the configmap %s in namespace %s", configCmName, configNamespace)
				return
			}
		}
		logrus.Info("passed in builder args: ", builderArgs)
	} else {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Configmap %s is not found in namespace %s", configCmName, configNamespace)
	}

	dockerFilePath := "/workspace/buildcontext/env/docker/Dockerfile"

	builderContainerEnvFrom := make([]corev1.EnvFromSource, 0)
	builderContainerEnvs := []corev1.EnvVar{
		{
			Name:  "DOCKER_CONFIG",
			Value: "/kaniko/.docker/",
		},
		{
			Name:  "IFS",
			Value: "''",
		},
	}

	if UsingAWSECRWithIAMRole() {
		builderContainerEnvs = append(builderContainerEnvs, corev1.EnvVar{
			Name:  "AWS_REGION",
			Value: GetAWSECRRegion(),
		}, corev1.EnvVar{
			Name:  "AWS_SDK_LOAD_CONFIG",
			Value: "true",
		})
	}

	kubeAnnotations := make(map[string]string)
	command := []string{
		"/kaniko/executor",
	}
	args := []string{
		"--context=/workspace/buildcontext",
		"--verbosity=info",
		"--cache=true",
		"--compressed-caching=false",
		fmt.Sprintf("--dockerfile=%s", dockerFilePath),
		fmt.Sprintf("--insecure=%v", dockerRegistryInsecure),
		fmt.Sprintf("--destination=%s", inClusterImageName),
	}
	var builderImage string
	switch buildEngine {
	case BentoImageBuildEngineKaniko:
		builderImage = internalImages.Kaniko
		if isEstargzEnabled() {
			builderContainerEnvs = append(builderContainerEnvs, corev1.EnvVar{
				Name:  "GGCR_EXPERIMENT_ESTARGZ",
				Value: "1",
			})
		}
	case BentoImageBuildEngineBuildkit:
		builderImage = internalImages.Buildkit
	case BentoImageBuildEngineBuildkitRootless:
		builderImage = internalImages.BuildkitRootless
	default:
		err = errors.Errorf("unknown bento image build engine %s", buildEngine)
		return
	}

	isBuildkit := buildEngine == BentoImageBuildEngineBuildkit || buildEngine == BentoImageBuildEngineBuildkitRootless

	if isBuildkit {
		buildkitdFlags := []string{}
		if !privileged {
			buildkitdFlags = append(buildkitdFlags, "--oci-worker-no-process-sandbox")
		}
		if isEstargzEnabled() {
			buildkitdFlags = append(buildkitdFlags, "--oci-worker-snapshotter=stargz")
		}
		if len(buildkitdFlags) > 0 {
			builderContainerEnvs = append(builderContainerEnvs, corev1.EnvVar{
				Name:  "BUILDKITD_FLAGS",
				Value: strings.Join(buildkitdFlags, " "),
			})
		}
		command = []string{"buildctl-daemonless.sh"}
		args = []string{
			"build",
			"--frontend",
			"dockerfile.v0",
			"--local",
			"context=/workspace/buildcontext",
			"--local",
			fmt.Sprintf("dockerfile=%s", filepath.Dir(dockerFilePath)),
			"--output",
			fmt.Sprintf("type=image,name=%s,push=true,registry.insecure=%v", inClusterImageName, dockerRegistryInsecure),
		}
		buildkitS3CacheEnabled := os.Getenv("BUILDKIT_S3_CACHE_ENABLED") == commonconsts.KubeLabelValueTrue
		if buildkitS3CacheEnabled {
			buildkitS3CacheRegion := os.Getenv("BUILDKIT_S3_CACHE_REGION")
			buildkitS3CacheBucket := os.Getenv("BUILDKIT_S3_CACHE_BUCKET")
			cachedImageName := bentoRepositoryName
			if isAddNamespacePrefix() {
				cachedImageName = fmt.Sprintf("%s.%s", opt.BentoRequest.Namespace, bentoRepositoryName)
			}
			args = append(args, "--cache-from", fmt.Sprintf("type=s3,region=%s,bucket=%s,name=%s", buildkitS3CacheRegion, buildkitS3CacheBucket, cachedImageName))
			args = append(args, "--cache-to", fmt.Sprintf("type=s3,region=%s,bucket=%s,name=%s,mode=max,compression=zstd,ignore-error=true", buildkitS3CacheRegion, buildkitS3CacheBucket, cachedImageName))
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
			if awsAccessKeySecretName != "" {
				builderContainerEnvFrom = append(builderContainerEnvFrom, corev1.EnvFromSource{
					SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: awsAccessKeySecretName,
						},
					},
				})
			}
		}
	}

	var builderContainerSecurityContext *corev1.SecurityContext

	if buildEngine == BentoImageBuildEngineBuildkit {
		builderContainerSecurityContext = &corev1.SecurityContext{
			Privileged: pointer.BoolPtr(true),
		}
	} else if buildEngine == BentoImageBuildEngineBuildkitRootless {
		kubeAnnotations["container.apparmor.security.beta.kubernetes.io/builder"] = "unconfined"
		builderContainerSecurityContext = &corev1.SecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined,
			},
			RunAsUser:  pointer.Int64Ptr(1000),
			RunAsGroup: pointer.Int64Ptr(1000),
		}
	}

	// add build args to pass via --build-arg
	for _, buildArg := range buildArgs {
		if isBuildkit {
			args = append(args, "--opt", fmt.Sprintf("build-arg:%s", buildArg))
		} else {
			args = append(args, fmt.Sprintf("--build-arg=%s", buildArg))
		}
	}
	// add other arguments to builder
	args = append(args, builderArgs...)
	logrus.Info("yatai-image-builder args: ", args)

	// nolint: gosec
	buildArgsSecretName := "yatai-image-builder-build-args"
	r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting secret %s from namespace %s", buildArgsSecretName, configNamespace)
	buildArgsSecret := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: buildArgsSecretName, Namespace: configNamespace}, buildArgsSecret)
	buildArgsSecretIsNotFound := k8serrors.IsNotFound(err)
	if err != nil && !buildArgsSecretIsNotFound {
		err = errors.Wrap(err, "failed to get secret")
		return
	}

	if !buildArgsSecretIsNotFound {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is got from namespace %s", buildArgsSecretName, configNamespace)
		if configNamespace != opt.BentoRequest.Namespace {
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is in namespace %s, but BentoRequest is in namespace %s, so we need to copy the secret to BentoRequest namespace", buildArgsSecretName, configNamespace, opt.BentoRequest.Namespace)
			r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Getting secret %s in namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
			_buildArgsSecret := &corev1.Secret{}
			err = r.Get(ctx, types.NamespacedName{Namespace: opt.BentoRequest.Namespace, Name: buildArgsSecretName}, _buildArgsSecret)
			localBuildArgsSecretIsNotFound := k8serrors.IsNotFound(err)
			if err != nil && !localBuildArgsSecretIsNotFound {
				err = errors.Wrapf(err, "failed to get secret %s from namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
				return
			}
			if localBuildArgsSecretIsNotFound {
				r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Copying secret %s from namespace %s to namespace %s", buildArgsSecretName, configNamespace, opt.BentoRequest.Namespace)
				err = r.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      buildArgsSecretName,
						Namespace: opt.BentoRequest.Namespace,
					},
					Data: buildArgsSecret.Data,
				})
				if err != nil {
					err = errors.Wrapf(err, "failed to create secret %s in namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
					return
				}
			} else {
				r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is already in namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
				r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Updating secret %s in namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
				err = r.Update(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      buildArgsSecretName,
						Namespace: opt.BentoRequest.Namespace,
					},
					Data: buildArgsSecret.Data,
				})
				if err != nil {
					err = errors.Wrapf(err, "failed to update secret %s in namespace %s", buildArgsSecretName, opt.BentoRequest.Namespace)
					return
				}
			}
		}

		for key := range buildArgsSecret.Data {
			envName := fmt.Sprintf("BENTOML_BUILD_ARG_%s", strings.ReplaceAll(strings.ToUpper(strcase.ToKebab(key)), "-", "_"))
			builderContainerEnvs = append(builderContainerEnvs, corev1.EnvVar{
				Name: envName,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: buildArgsSecretName,
						},
						Key: key,
					},
				},
			})

			if isBuildkit {
				args = append(args, "--opt", fmt.Sprintf("build-arg:%s=$(%s)", key, envName))
			} else {
				args = append(args, fmt.Sprintf("--build-arg=%s=$(%s)", key, envName))
			}
		}
	} else {
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Secret %s is not found in namespace %s", buildArgsSecretName, configNamespace)
	}

	builderContainerArgs := []string{
		"-c",
		fmt.Sprintf("%s %s && exit 0 || exit %d", strings.Join(command, " "), strings.Join(args, " "), BuilderJobFailedExitCode),
	}

	container := corev1.Container{
		Name:            BuilderContainerName,
		Image:           builderImage,
		ImagePullPolicy: corev1.PullAlways,
		Command:         []string{"sh"},
		Args:            builderContainerArgs,
		VolumeMounts:    volumeMounts,
		Env:             builderContainerEnvs,
		EnvFrom:         builderContainerEnvFrom,
		TTY:             true,
		Stdin:           true,
		SecurityContext: builderContainerSecurityContext,
	}

	if globalDefaultImageBuilderContainerResources != nil {
		container.Resources = *globalDefaultImageBuilderContainerResources
	}

	if opt.BentoRequest.Spec.ImageBuilderContainerResources != nil {
		container.Resources = *opt.BentoRequest.Spec.ImageBuilderContainerResources
	}

	containers = append(containers, container)

	pod = &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      kubeLabels,
			Annotations: kubeAnnotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			Volumes:        volumes,
			InitContainers: initContainers,
			Containers:     containers,
		},
	}

	if globalExtraPodMetadata != nil {
		for k, v := range globalExtraPodMetadata.Annotations {
			pod.Annotations[k] = v
		}

		for k, v := range globalExtraPodMetadata.Labels {
			pod.Labels[k] = v
		}
	}

	if opt.BentoRequest.Spec.ImageBuilderExtraPodMetadata != nil {
		for k, v := range opt.BentoRequest.Spec.ImageBuilderExtraPodMetadata.Annotations {
			pod.Annotations[k] = v
		}

		for k, v := range opt.BentoRequest.Spec.ImageBuilderExtraPodMetadata.Labels {
			pod.Labels[k] = v
		}
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

	if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec != nil {
		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.PriorityClassName != "" {
			pod.Spec.PriorityClassName = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.PriorityClassName
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.SchedulerName != "" {
			pod.Spec.SchedulerName = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.SchedulerName
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.NodeSelector != nil {
			pod.Spec.NodeSelector = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.NodeSelector
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.Affinity != nil {
			pod.Spec.Affinity = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.Affinity
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.Tolerations != nil {
			pod.Spec.Tolerations = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.Tolerations
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.TopologySpreadConstraints != nil {
			pod.Spec.TopologySpreadConstraints = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.TopologySpreadConstraints
		}

		if opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.ServiceAccountName != "" {
			pod.Spec.ServiceAccountName = opt.BentoRequest.Spec.ImageBuilderExtraPodSpec.ServiceAccountName
		}
	}

	if pod.Spec.ServiceAccountName == "" {
		serviceAccounts := &corev1.ServiceAccountList{}
		err = r.List(ctx, serviceAccounts, client.InNamespace(opt.BentoRequest.Namespace), client.MatchingLabels{
			commonconsts.KubeLabelYataiImageBuilderPod: commonconsts.KubeLabelValueTrue,
		})
		if err != nil {
			err = errors.Wrapf(err, "failed to list service accounts in namespace %s", opt.BentoRequest.Namespace)
			return
		}
		if len(serviceAccounts.Items) > 0 {
			pod.Spec.ServiceAccountName = serviceAccounts.Items[0].Name
		} else {
			pod.Spec.ServiceAccountName = "default"
		}
	}

	for i, c := range pod.Spec.InitContainers {
		env := c.Env
		if globalExtraContainerEnv != nil {
			env = append(env, globalExtraContainerEnv...)
		}
		env = append(env, opt.BentoRequest.Spec.ImageBuilderExtraContainerEnv...)
		pod.Spec.InitContainers[i].Env = env
	}
	for i, c := range pod.Spec.Containers {
		env := c.Env
		if globalExtraContainerEnv != nil {
			env = append(env, globalExtraContainerEnv...)
		}
		env = append(env, opt.BentoRequest.Spec.ImageBuilderExtraContainerEnv...)
		pod.Spec.Containers[i].Env = env
	}

	return
}
