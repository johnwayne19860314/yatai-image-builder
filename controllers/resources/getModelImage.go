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

	"github.com/pkg/errors"
	"github.com/rs/xid"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/huandu/xstrings"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
)

func (r *BentoRequestReconciler) makeSureDockerConfigJSONSecret(ctx context.Context, namespace string, dockerRegistryConf *commonconfig.DockerRegistryConfig) (dockerConfigJSONSecret *corev1.Secret, err error) {
	if dockerRegistryConf.Username == "" {
		return
	}

	// nolint: gosec
	dockerConfigSecretName := commonconsts.KubeSecretNameRegcred
	dockerConfigObj := struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}{
		Auths: map[string]struct {
			Auth string `json:"auth"`
		}{
			dockerRegistryConf.Server: {
				Auth: base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", dockerRegistryConf.Username, dockerRegistryConf.Password))),
			},
		},
	}

	dockerConfigContent, err := json.Marshal(dockerConfigObj)
	if err != nil {
		err = errors.Wrap(err, "marshal docker config")
		return nil, err
	}

	dockerConfigJSONSecret = &corev1.Secret{}

	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dockerConfigSecretName}, dockerConfigJSONSecret)
	dockerConfigIsNotFound := k8serrors.IsNotFound(err)
	// nolint: gocritic
	if err != nil && !dockerConfigIsNotFound {
		err = errors.Wrap(err, "get docker config secret")
		return nil, err
	}
	err = nil
	if dockerConfigIsNotFound {
		dockerConfigJSONSecret = &corev1.Secret{
			Type: corev1.SecretTypeDockerConfigJson,
			ObjectMeta: metav1.ObjectMeta{
				Name:      dockerConfigSecretName,
				Namespace: namespace,
			},
			Data: map[string][]byte{
				".dockerconfigjson": dockerConfigContent,
			},
		}
		err_ := r.Create(ctx, dockerConfigJSONSecret)
		if err_ != nil {
			dockerConfigJSONSecret = &corev1.Secret{}
			err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: dockerConfigSecretName}, dockerConfigJSONSecret)
			dockerConfigIsNotFound = k8serrors.IsNotFound(err)
			if err != nil && !dockerConfigIsNotFound {
				err = errors.Wrap(err, "get docker config secret")
				return nil, err
			}
			if dockerConfigIsNotFound {
				err_ = errors.Wrap(err_, "create docker config secret")
				return nil, err_
			}
			if err != nil {
				err = nil
			}
		}
	} else {
		dockerConfigJSONSecret.Data[".dockerconfigjson"] = dockerConfigContent
		err = r.Update(ctx, dockerConfigJSONSecret)
		if err != nil {
			err = errors.Wrap(err, "update docker config secret")
			return nil, err
		}
	}

	return
}

func (r *BentoRequestReconciler) getDockerRegistry(ctx context.Context, bentoRequest *resourcesv1alpha1.BentoRequest) (dockerRegistry modelschemas.DockerRegistrySchema, err error) {
	if bentoRequest != nil && bentoRequest.Spec.DockerConfigJSONSecretName != "" {
		secret := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{
			Namespace: bentoRequest.Namespace,
			Name:      bentoRequest.Spec.DockerConfigJSONSecretName,
		}, secret)
		if err != nil {
			err = errors.Wrapf(err, "get docker config json secret %s", bentoRequest.Spec.DockerConfigJSONSecretName)
			return
		}
		configJSON, ok := secret.Data[".dockerconfigjson"]
		if !ok {
			err = errors.Errorf("docker config json secret %s does not have .dockerconfigjson key", bentoRequest.Spec.DockerConfigJSONSecretName)
			return
		}
		var configObj struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		err = json.Unmarshal(configJSON, &configObj)
		if err != nil {
			err = errors.Wrapf(err, "unmarshal docker config json secret %s", bentoRequest.Spec.DockerConfigJSONSecretName)
			return
		}
		imageRegistryURI, _, _ := xstrings.Partition(bentoRequest.Spec.Image, "/")
		var server string
		var auth string
		if imageRegistryURI != "" {
			for k, v := range configObj.Auths {
				if k == imageRegistryURI {
					server = k
					auth = v.Auth
					break
				}
			}
			if server == "" {
				for k, v := range configObj.Auths {
					if strings.Contains(k, imageRegistryURI) {
						server = k
						auth = v.Auth
						break
					}
				}
			}
		}
		if server == "" {
			for k, v := range configObj.Auths {
				server = k
				auth = v.Auth
				break
			}
		}
		if server == "" {
			err = errors.Errorf("no auth in docker config json secret %s", bentoRequest.Spec.DockerConfigJSONSecretName)
			return
		}
		dockerRegistry.Server = server
		var credentials []byte
		credentials, err = base64.StdEncoding.DecodeString(auth)
		if err != nil {
			err = errors.Wrapf(err, "cannot base64 decode auth in docker config json secret %s", bentoRequest.Spec.DockerConfigJSONSecretName)
			return
		}
		dockerRegistry.Username, _, dockerRegistry.Password = xstrings.Partition(string(credentials), ":")
		if bentoRequest.Spec.OCIRegistryInsecure != nil {
			dockerRegistry.Secure = !*bentoRequest.Spec.OCIRegistryInsecure
		}
		return
	}

	dockerRegistryConfig, err := commonconfig.GetDockerRegistryConfig(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, secret)
		return secret, errors.Wrap(err, "get secret")
	})
	if err != nil {
		err = errors.Wrap(err, "get docker registry")
		return
	}

	bentoRepositoryName := "yatai-bentos"
	modelRepositoryName := "yatai-models"
	if dockerRegistryConfig.BentoRepositoryName != "" {
		bentoRepositoryName = dockerRegistryConfig.BentoRepositoryName
	}
	if dockerRegistryConfig.ModelRepositoryName != "" {
		modelRepositoryName = dockerRegistryConfig.ModelRepositoryName
	}
	bentoRepositoryURI := fmt.Sprintf("%s/%s", strings.TrimRight(dockerRegistryConfig.Server, "/"), bentoRepositoryName)
	modelRepositoryURI := fmt.Sprintf("%s/%s", strings.TrimRight(dockerRegistryConfig.Server, "/"), modelRepositoryName)
	if strings.Contains(dockerRegistryConfig.Server, "docker.io") {
		bentoRepositoryURI = fmt.Sprintf("docker.io/%s", bentoRepositoryName)
		modelRepositoryURI = fmt.Sprintf("docker.io/%s", modelRepositoryName)
	}
	bentoRepositoryInClusterURI := bentoRepositoryURI
	modelRepositoryInClusterURI := modelRepositoryURI
	if dockerRegistryConfig.InClusterServer != "" {
		bentoRepositoryInClusterURI = fmt.Sprintf("%s/%s", strings.TrimRight(dockerRegistryConfig.InClusterServer, "/"), bentoRepositoryName)
		modelRepositoryInClusterURI = fmt.Sprintf("%s/%s", strings.TrimRight(dockerRegistryConfig.InClusterServer, "/"), modelRepositoryName)
		if strings.Contains(dockerRegistryConfig.InClusterServer, "docker.io") {
			bentoRepositoryInClusterURI = fmt.Sprintf("docker.io/%s", bentoRepositoryName)
			modelRepositoryInClusterURI = fmt.Sprintf("docker.io/%s", modelRepositoryName)
		}
	}
	dockerRegistry = modelschemas.DockerRegistrySchema{
		Server:                       dockerRegistryConfig.Server,
		Username:                     dockerRegistryConfig.Username,
		Password:                     dockerRegistryConfig.Password,
		Secure:                       dockerRegistryConfig.Secure,
		BentosRepositoryURI:          bentoRepositoryURI,
		BentosRepositoryURIInCluster: bentoRepositoryInClusterURI,
		ModelsRepositoryURI:          modelRepositoryURI,
		ModelsRepositoryURIInCluster: modelRepositoryInClusterURI,
	}

	return
}

func (r *BentoRequestReconciler) getImageInfo(ctx context.Context, opt GetImageInfoOption) (imageInfo ImageInfo, err error) {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(opt.BentoRequest.Spec.BentoTag, ":")
	if err != nil {
		err = errors.Wrap(err, "create kubernetes clientset")
		return
	}
	dockerRegistry, err := r.getDockerRegistry(ctx, opt.BentoRequest)
	if err != nil {
		err = errors.Wrap(err, "get docker registry")
		return
	}
	imageInfo.DockerRegistry = dockerRegistry
	imageInfo.ImageName = getBentoImageName(opt.BentoRequest, dockerRegistry, bentoRepositoryName, bentoVersion, false)
	imageInfo.InClusterImageName = getBentoImageName(opt.BentoRequest, dockerRegistry, bentoRepositoryName, bentoVersion, true)

	imageInfo.DockerConfigJSONSecretName = opt.BentoRequest.Spec.DockerConfigJSONSecretName

	imageInfo.DockerRegistryInsecure = opt.BentoRequest.Annotations[commonconsts.KubeAnnotationDockerRegistryInsecure] == "true"
	if opt.BentoRequest.Spec.OCIRegistryInsecure != nil {
		imageInfo.DockerRegistryInsecure = *opt.BentoRequest.Spec.OCIRegistryInsecure
	}

	if imageInfo.DockerConfigJSONSecretName == "" {
		var dockerRegistryConf *commonconfig.DockerRegistryConfig
		dockerRegistryConf, err = commonconfig.GetDockerRegistryConfig(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
			secret := &corev1.Secret{}
			err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, secret)
			return secret, errors.Wrap(err, "get docker registry secret")
		})
		if err != nil {
			err = errors.Wrap(err, "get docker registry")
			return
		}
		imageInfo.DockerRegistryInsecure = !dockerRegistryConf.Secure
		var dockerConfigSecret *corev1.Secret
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Making sure docker config secret %s in namespace %s", commonconsts.KubeSecretNameRegcred, opt.BentoRequest.Namespace)
		dockerConfigSecret, err = r.makeSureDockerConfigJSONSecret(ctx, opt.BentoRequest.Namespace, dockerRegistryConf)
		if err != nil {
			err = errors.Wrap(err, "make sure docker config secret")
			return
		}
		r.Recorder.Eventf(opt.BentoRequest, corev1.EventTypeNormal, "GenerateImageBuilderPod", "Docker config secret %s in namespace %s is ready", commonconsts.KubeSecretNameRegcred, opt.BentoRequest.Namespace)
		if dockerConfigSecret != nil {
			imageInfo.DockerConfigJSONSecretName = dockerConfigSecret.Name
		}
	}
	return
}

func (r *BentoRequestReconciler) getBento(ctx context.Context, bentoRequest *resourcesv1alpha1.BentoRequest) (bento *schemasv1.BentoFullSchema, err error) {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bentoRequest.Spec.BentoTag, ":")

	yataiClient_, _, err := r.getYataiClient(ctx)
	if err != nil {
		err = errors.Wrap(err, "get yatai client")
		return
	}

	if yataiClient_ == nil {
		err = errors.New("can't get yatai client, please check yatai configuration")
		return
	}

	yataiClient := *yataiClient_

	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "FetchBento", "Getting bento %s from yatai service", bentoRequest.Spec.BentoTag)
	bento, err = yataiClient.GetBento(ctx, bentoRepositoryName, bentoVersion)
	if err != nil {
		err = errors.Wrap(err, "get bento")
		return
	}
	r.Recorder.Eventf(bentoRequest, corev1.EventTypeNormal, "FetchBento", "Got bento %s from yatai service", bentoRequest.Spec.BentoTag)
	return
}

func (r *BentoRequestReconciler) getImageBuilderJobName() string {
	guid := xid.New()
	return fmt.Sprintf("yatai-bento-image-builder-%s", guid.String())
}

func (r *BentoRequestReconciler) getModelSeederJobName() string {
	guid := xid.New()
	return fmt.Sprintf("yatai-model-seeder-%s", guid.String())
}

func (r *BentoRequestReconciler) getModelSeederJobLabels(bentoRequest *resourcesv1alpha1.BentoRequest, model *resourcesv1alpha1.BentoModel) map[string]string {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bentoRequest.Spec.BentoTag, ":")
	modelRepositoryName, _, modelVersion := xstrings.Partition(model.Tag, ":")
	return map[string]string{
		commonconsts.KubeLabelBentoRequest:         bentoRequest.Name,
		commonconsts.KubeLabelIsModelSeeder:        "true",
		commonconsts.KubeLabelYataiModelRepository: modelRepositoryName,
		commonconsts.KubeLabelYataiModel:           modelVersion,
		commonconsts.KubeLabelYataiBentoRepository: bentoRepositoryName,
		commonconsts.KubeLabelYataiBento:           bentoVersion,
	}
}

func (r *BentoRequestReconciler) getModelSeederPodLabels(bentoRequest *resourcesv1alpha1.BentoRequest, model *resourcesv1alpha1.BentoModel) map[string]string {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bentoRequest.Spec.BentoTag, ":")
	modelRepositoryName, _, modelVersion := xstrings.Partition(model.Tag, ":")
	return map[string]string{
		commonconsts.KubeLabelBentoRequest:         bentoRequest.Name,
		commonconsts.KubeLabelIsModelSeeder:        "true",
		commonconsts.KubeLabelIsBentoImageBuilder:  "true",
		commonconsts.KubeLabelYataiModelRepository: modelRepositoryName,
		commonconsts.KubeLabelYataiModel:           modelVersion,
		commonconsts.KubeLabelYataiBentoRepository: bentoRepositoryName,
		commonconsts.KubeLabelYataiBento:           bentoVersion,
	}
}

func (r *BentoRequestReconciler) getImageBuilderJobLabels(bentoRequest *resourcesv1alpha1.BentoRequest) map[string]string {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bentoRequest.Spec.BentoTag, ":")
	labels := map[string]string{
		commonconsts.KubeLabelBentoRequest:         bentoRequest.Name,
		commonconsts.KubeLabelIsBentoImageBuilder:  "true",
		commonconsts.KubeLabelYataiBentoRepository: bentoRepositoryName,
		commonconsts.KubeLabelYataiBento:           bentoVersion,
	}

	if isSeparateModels(bentoRequest) {
		labels[KubeLabelYataiImageBuilderSeparateModels] = commonconsts.KubeLabelValueTrue
	} else {
		labels[KubeLabelYataiImageBuilderSeparateModels] = commonconsts.KubeLabelValueFalse
	}
	return labels
}

func (r *BentoRequestReconciler) getImageBuilderPodLabels(bentoRequest *resourcesv1alpha1.BentoRequest) map[string]string {
	bentoRepositoryName, _, bentoVersion := xstrings.Partition(bentoRequest.Spec.BentoTag, ":")
	return map[string]string{
		commonconsts.KubeLabelBentoRequest:         bentoRequest.Name,
		commonconsts.KubeLabelIsBentoImageBuilder:  "true",
		commonconsts.KubeLabelYataiBentoRepository: bentoRepositoryName,
		commonconsts.KubeLabelYataiBento:           bentoVersion,
	}
}

func (r *BentoRequestReconciler) getModelPVCName(bentoRequest *resourcesv1alpha1.BentoRequest, model *resourcesv1alpha1.BentoModel) string {
	storageClassName := getJuiceFSStorageClassName()
	var hashStr string
	if isAddNamespacePrefix() {
		hashStr = hash(fmt.Sprintf("%s:%s:%s", storageClassName, bentoRequest.Namespace, model.Tag))
	} else {
		hashStr = hash(fmt.Sprintf("%s:%s", storageClassName, model.Tag))
	}
	pvcName := fmt.Sprintf("model-seeder-%s", hashStr)
	if len(pvcName) > 63 {
		pvcName = pvcName[:63]
	}
	return pvcName
}

func (r *BentoRequestReconciler) getJuiceFSModelPath(bentoRequest *resourcesv1alpha1.BentoRequest, model *resourcesv1alpha1.BentoModel) string {
	modelRepositoryName, _, modelVersion := xstrings.Partition(model.Tag, ":")
	path := fmt.Sprintf("models/.shared/%s/%s", modelRepositoryName, modelVersion)
	if isAddNamespacePrefix() {
		path = fmt.Sprintf("models/%s/%s/%s", bentoRequest.Namespace, modelRepositoryName, modelVersion)
	}
	return path
}
