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

	"strconv"

	"fmt"
	"time"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	commonconfig "github.com/bentoml/yatai-common/config"
	commonconsts "github.com/bentoml/yatai-common/consts"
	"github.com/bentoml/yatai-schemas/modelschemas"
	"github.com/bentoml/yatai-schemas/schemasv1"

	resourcesv1alpha1 "github.com/bentoml/yatai-image-builder/apis/resources/v1alpha1"
	"github.com/bentoml/yatai-image-builder/version"
	yataiclient "github.com/bentoml/yatai-image-builder/yatai-client"
)

func (r *BentoRequestReconciler) getYataiClient(ctx context.Context) (yataiClient **yataiclient.YataiClient, yataiConf **commonconfig.YataiConfig, err error) {
	yataiConf_, err := commonconfig.GetYataiConfig(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, secret)
		return secret, errors.Wrap(err, "get secret")
	}, commonconsts.YataiImageBuilderComponentName, true)
	isNotFound := k8serrors.IsNotFound(err)
	if err != nil && !isNotFound {
		err = errors.Wrap(err, "get yatai config")
		return
	}

	if isNotFound {
		return
	}

	if yataiConf_.Endpoint == "" {
		return
	}

	if yataiConf_.ClusterName == "" {
		yataiConf_.ClusterName = "default"
	}

	yataiClient_ := yataiclient.NewYataiClient(yataiConf_.Endpoint, fmt.Sprintf("%s:%s:%s", commonconsts.YataiImageBuilderComponentName, yataiConf_.ClusterName, yataiConf_.ApiToken))

	yataiClient = &yataiClient_
	yataiConf = &yataiConf_
	return
}

func (r *BentoRequestReconciler) getHashStr(bentoRequest *resourcesv1alpha1.BentoRequest) (string, error) {
	var hash uint64
	hash, err := hashstructure.Hash(struct {
		Spec        resourcesv1alpha1.BentoRequestSpec
		Labels      map[string]string
		Annotations map[string]string
	}{
		Spec:        bentoRequest.Spec,
		Labels:      bentoRequest.Labels,
		Annotations: bentoRequest.Annotations,
	}, hashstructure.FormatV2, nil)
	if err != nil {
		err = errors.Wrap(err, "get bentoRequest CR spec hash")
		return "", err
	}
	hashStr := strconv.FormatUint(hash, 10)
	return hashStr, nil
}

func (r *BentoRequestReconciler) doRegisterYataiComponent() (err error) {
	logs := log.Log.WithValues("func", "doRegisterYataiComponent")

	ctx, cancel := context.WithTimeout(context.TODO(), time.Minute*5)
	defer cancel()

	logs.Info("getting yatai client")
	yataiClient, yataiConf, err := r.getYataiClient(ctx)
	if err != nil {
		err = errors.Wrap(err, "get yatai client")
		return
	}

	if yataiClient == nil || yataiConf == nil {
		logs.Info("can't get yatai client, skip registering")
		return
	}

	yataiClient_ := *yataiClient
	yataiConf_ := *yataiConf

	namespace, err := commonconfig.GetYataiImageBuilderNamespace(ctx, func(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: namespace,
			Name:      name,
		}, secret)
		return secret, errors.Wrap(err, "get secret")
	})
	if err != nil {
		err = errors.Wrap(err, "get yatai image builder namespace")
		return
	}

	_, err = yataiClient_.RegisterYataiComponent(ctx, yataiConf_.ClusterName, &schemasv1.RegisterYataiComponentSchema{
		Name:          modelschemas.YataiComponentNameImageBuilder,
		KubeNamespace: namespace,
		Version:       version.Version,
		SelectorLabels: map[string]string{
			"app.kubernetes.io/name": "yatai-image-builder",
		},
		Manifest: &modelschemas.YataiComponentManifestSchema{
			SelectorLabels: map[string]string{
				"app.kubernetes.io/name": "yatai-image-builder",
			},
			LatestCRDVersion: "v1alpha1",
		},
	})

	err = errors.Wrap(err, "register yatai component")
	return err
}

func (r *BentoRequestReconciler) registerYataiComponent() {
	logs := log.Log.WithValues("func", "registerYataiComponent")
	err := r.doRegisterYataiComponent()
	if err != nil {
		logs.Error(err, "registerYataiComponent")
	}
	ticker := time.NewTicker(time.Minute * 5)
	for range ticker.C {
		err := r.doRegisterYataiComponent()
		if err != nil {
			logs.Error(err, "registerYataiComponent")
		}
	}
}
