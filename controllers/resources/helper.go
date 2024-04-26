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

	// nolint: gosec
	"crypto/md5"

	"fmt"
	"os"

	"github.com/pkg/errors"

	"encoding/hex"
	"strings"

	"github.com/huandu/xstrings"
	"github.com/prune998/docker-registry-client/registry"
	"github.com/sirupsen/logrus"

	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/modelschemas"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
)

const (
	// nolint: gosec
	YataiImageBuilderAWSAccessKeySecretName  = "yatai-image-builder-aws-access-key"
	KubeAnnotationBentoRequestHash           = "yatai.ai/bento-request-hash"
	KubeLabelYataiImageBuilderSeparateModels = "yatai.ai/yatai-image-builder-separate-models"
)

func isEstargzEnabled() bool {
	return os.Getenv("ESTARGZ_ENABLED") == commonconsts.KubeLabelValueTrue
}

func UsingAWSECRWithIAMRole() bool {
	return os.Getenv(commonconsts.EnvAWSECRWithIAMRole) == commonconsts.KubeLabelValueTrue
}

func GetAWSECRRegion() string {
	return os.Getenv(commonconsts.EnvAWSECRRegion)
}

func CheckECRImageExists(imageName string) (bool, error) {
	region := GetAWSECRRegion()
	if region == "" {
		return false, fmt.Errorf("%s is not set", commonconsts.EnvAWSECRRegion)
	}

	sess, err := session.NewSession(&aws.Config{Region: aws.String(region)})
	if err != nil {
		return false, errors.Wrap(err, "create aws session")
	}

	_, _, imageName_ := xstrings.Partition(imageName, "/")
	repoName, _, tag := xstrings.Partition(imageName_, ":")

	svc := ecr.New(sess)
	input := &ecr.DescribeImagesInput{
		RepositoryName: aws.String(repoName),
		ImageIds: []*ecr.ImageIdentifier{
			{
				ImageTag: aws.String(tag),
			},
		},
	}

	_, err = svc.DescribeImages(input)
	if err != nil {
		var awsErr awserr.Error
		if errors.As(err, &awsErr) {
			if awsErr.Code() == "ImageNotFoundException" {
				return false, nil
			}
		}
		return false, errors.Wrap(err, "describe ECR images")
	}

	return true, nil
}

type BentoImageBuildEngine string

const (
	BentoImageBuildEngineKaniko           BentoImageBuildEngine = "kaniko"
	BentoImageBuildEngineBuildkit         BentoImageBuildEngine = "buildkit"
	BentoImageBuildEngineBuildkitRootless BentoImageBuildEngine = "buildkit-rootless"
)

const (
	EnvBentoImageBuildEngine = "BENTO_IMAGE_BUILD_ENGINE"
)

func getBentoImageBuildEngine() BentoImageBuildEngine {
	engine := os.Getenv(EnvBentoImageBuildEngine)
	if engine == "" {
		return BentoImageBuildEngineKaniko
	}
	return BentoImageBuildEngine(engine)
}

func isAddNamespacePrefix() bool {
	return os.Getenv("ADD_NAMESPACE_PREFIX_TO_IMAGE_NAME") == trueStr
}

func getBentoImageName(bentoRequest *resourcesv1alpha1.BentoRequest, dockerRegistry modelschemas.DockerRegistrySchema, bentoRepositoryName, bentoVersion string, inCluster bool) string {
	if bentoRequest != nil && bentoRequest.Spec.Image != "" {
		return bentoRequest.Spec.Image
	}
	var uri, tag string
	if inCluster {
		uri = dockerRegistry.BentosRepositoryURIInCluster
	} else {
		uri = dockerRegistry.BentosRepositoryURI
	}
	tail := fmt.Sprintf("%s.%s", bentoRepositoryName, bentoVersion)
	separateModels := isSeparateModels(bentoRequest)
	if separateModels {
		tail += ".nomodels"
	}
	if isEstargzEnabled() {
		tail += ".esgz"
	}
	if isAddNamespacePrefix() {
		tag = fmt.Sprintf("yatai.%s.%s", bentoRequest.Namespace, tail)
	} else {
		tag = fmt.Sprintf("yatai.%s", tail)
	}
	if len(tag) > 128 {
		hashStr := hash(tail)
		if isAddNamespacePrefix() {
			tag = fmt.Sprintf("yatai.%s.%s", bentoRequest.Namespace, hashStr)
		} else {
			tag = fmt.Sprintf("yatai.%s", hashStr)
		}
		if len(tag) > 128 {
			if isAddNamespacePrefix() {
				tag = fmt.Sprintf("yatai.%s", hash(fmt.Sprintf("%s.%s", bentoRequest.Namespace, tail)))[:128]
			} else {
				tag = fmt.Sprintf("yatai.%s", hash(tail))[:128]
			}
		}
	}
	return fmt.Sprintf("%s:%s", uri, tag)
}

func isSeparateModels(bentoRequest *resourcesv1alpha1.BentoRequest) (separateModels bool) {
	return bentoRequest.Annotations[commonconsts.KubeAnnotationYataiImageBuilderSeparateModels] == commonconsts.KubeLabelValueTrue
}

func checkImageExists(bentoRequest *resourcesv1alpha1.BentoRequest, dockerRegistry modelschemas.DockerRegistrySchema, imageName string) (bool, error) {
	if bentoRequest.Annotations["yatai.ai/force-build-image"] == commonconsts.KubeLabelValueTrue {
		return false, nil
	}

	if UsingAWSECRWithIAMRole() {
		return CheckECRImageExists(imageName)
	}

	server, _, imageName := xstrings.Partition(imageName, "/")
	if strings.Contains(server, "docker.io") {
		server = "index.docker.io"
	}

	if dockerRegistry.Secure {
		server = fmt.Sprintf("https://%s", server)
	} else {
		server = fmt.Sprintf("http://%s", server)
	}
	//server = "https://hub.docker.com/repositories/19860314"
	hub, err := registry.New(server, dockerRegistry.Username, dockerRegistry.Password, logrus.Debugf)
	if err != nil {
		err = errors.Wrapf(err, "create docker registry client for %s", server)
		return false, err
	}
	imageName, _, tag := xstrings.LastPartition(imageName, ":")
	tags, err := hub.Tags(imageName)
	isNotFound := err != nil && strings.Contains(err.Error(), "404")
	if isNotFound {
		return false, nil
	}
	if err != nil {
		err = errors.Wrapf(err, "get tags for docker image %s", imageName)
		return false, err
	}
	for _, tag_ := range tags {
		if tag_ == tag {
			return true, nil
		}
	}
	return false, nil
}

type ImageInfo struct {
	DockerRegistry             modelschemas.DockerRegistrySchema
	DockerConfigJSONSecretName string
	ImageName                  string
	InClusterImageName         string
	DockerRegistryInsecure     bool
}

type GetImageInfoOption struct {
	BentoRequest *resourcesv1alpha1.BentoRequest
}

func hash(text string) string {
	// nolint: gosec
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}

type GenerateModelPVCOption struct {
	BentoRequest *resourcesv1alpha1.BentoRequest
	Model        *resourcesv1alpha1.BentoModel
}

type GenerateModelSeederJobOption struct {
	BentoRequest *resourcesv1alpha1.BentoRequest
	Model        *resourcesv1alpha1.BentoModel
}

type GenerateModelSeederPodTemplateSpecOption struct {
	BentoRequest *resourcesv1alpha1.BentoRequest
	Model        *resourcesv1alpha1.BentoModel
}

type GenerateImageBuilderJobOption struct {
	ImageInfo    ImageInfo
	BentoRequest *resourcesv1alpha1.BentoRequest
}

const BuilderContainerName = "builder"
const BuilderJobFailedExitCode = 42
const ModelSeederContainerName = "seeder"
const ModelSeederJobFailedExitCode = 42

type GenerateImageBuilderPodTemplateSpecOption struct {
	ImageInfo    ImageInfo
	BentoRequest *resourcesv1alpha1.BentoRequest
}

func getJuiceFSStorageClassName() string {
	if v := os.Getenv("JUICEFS_STORAGE_CLASS_NAME"); v != "" {
		return v
	}
	return "juicefs-sc"
}

const (
	trueStr = "true"
)
