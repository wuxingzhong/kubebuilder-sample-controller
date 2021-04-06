/*
Copyright 2019 The Kubernetes Authors.

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

	"github.com/go-logr/logr"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mygroupv1beta1 "jetstack.io/example-controller/api/v1beta1"
)

// MyKindReconciler reconciles a MyKind object
type MyKindReconciler struct {
	client.Client
	Log logr.Logger

	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=mygroup.k8s.io,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.k8s.io,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.k8s.io,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *MyKindReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("mykind", req.NamespacedName)

	// 控制器逻辑
	log.Info("获取自定义MyKind资源...")
	myKind := mygroupv1beta1.MyKind{}
	// 获取自定义控制器资源
	if err := r.Client.Get(ctx, req.NamespacedName, &myKind); err != nil {
		log.Error(err, "获取自定义MyKind资源失败!")
		// Ignore NotFound errors as they will be retried automatically if the
		// resource is created in future.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// 清理资源
	if err := r.cleanupOwnedResources(ctx, log, &myKind); err != nil {
		log.Error(err, "为MyKind资源管理器清理旧的Deployment资源失败!")
		return ctrl.Result{}, err
	}

	log = log.WithValues("deployment_name", myKind.Spec.DeploymentName)

	log.Info("检查是否存在被Mykind控制的Deployment资源")
	deployment := apps.Deployment{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: myKind.Namespace, Name: myKind.Spec.DeploymentName}, &deployment)
	if apierrors.IsNotFound(err) {
		log.Info("未找到被Mykind控制的Deployment资源, 创建一个Deployment..")

		deployment = *buildDeployment(myKind)
		// 创建deployment资源
		if err := r.Client.Create(ctx, &deployment); err != nil {
			log.Error(err, "创建Deployment资源失败!")
			return ctrl.Result{}, err
		}
		// 记录事件
		r.Recorder.Eventf(&myKind, core.EventTypeNormal, "Created", "创建 deployment %q", deployment.Name)
		log.Info("为MyKind控制器创建Deployment资源")
		return ctrl.Result{}, nil
	}
	if err != nil {
		log.Error(err, "获取MyKind控制的Deployment失败!")
		return ctrl.Result{}, err
	}

	log.Info("更新已经存在被Mykind控制的Deployment资源检查副本(replica)数量..")

	// 默认副本数量为1
	expectedReplicas := int32(1)
	if myKind.Spec.Replicas != nil {
		// 使用配置中的副本数量
		expectedReplicas = *myKind.Spec.Replicas
	}
	if *deployment.Spec.Replicas != expectedReplicas {
		log.Info("更新副本数量", "old_count", *deployment.Spec.Replicas, "new_count", expectedReplicas)

		deployment.Spec.Replicas = &expectedReplicas
		// 更新
		if err := r.Client.Update(ctx, &deployment); err != nil {
			log.Error(err, "更新Deployment副本数量失败!")
			return ctrl.Result{}, err
		}
		// 记录事件
		r.Recorder.Eventf(&myKind, core.EventTypeNormal, "Scaled", "扩缩 deployment %q to %d replicas", deployment.Name, expectedReplicas)

		return ctrl.Result{}, nil
	}

	log.Info("副本数量更新", "replica_count", *deployment.Spec.Replicas)

	log.Info("更新 MyKind控制器资源状态")
	myKind.Status.ReadyReplicas = deployment.Status.ReadyReplicas
	log.Info("MyKind控制器资源状态", "ReadyReplicas", myKind.Status.ReadyReplicas)
	// 更新
	if r.Client.Status().Update(ctx, &myKind); err != nil {
		log.Error(err, "failed to update MyKind status")
		return ctrl.Result{}, err
	}

	log.Info("资源状态同步完成")

	return ctrl.Result{}, nil
}

// cleanupOwnedResources will Delete any existing Deployment resources that
// were created for the given MyKind that no longer match the
// myKind.spec.deploymentName field.
// cleanupOwnedResources 清理任何被Mykind控制器控制的已经存在的,但和 myKind.spec.deploymentName不匹配的Deployment资源
func (r *MyKindReconciler) cleanupOwnedResources(ctx context.Context, log logr.Logger, myKind *mygroupv1beta1.MyKind) error {
	log.Info("查找被MyKind控制器控制的Deployments资源")

	// 列出所有被MyKind控制器控制的Deployments资源
	var deployments apps.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(myKind.Namespace), client.MatchingField(deploymentOwnerKey, myKind.Name)); err != nil {
		return err
	}

	deleted := 0
	for _, depl := range deployments.Items {
		if depl.Name == myKind.Spec.DeploymentName {
			// 如果资源名称和 MyKind控制名称相同,则不需删除
			continue
		}

		if err := r.Client.Delete(ctx, &depl); err != nil {
			log.Error(err, "删除Deployment资源失败!")
			return err
		}

		r.Recorder.Eventf(myKind, core.EventTypeNormal, "Deleted", "删除 deployment %q", depl.Name)
		deleted++
	}

	log.Info("完成清理旧的deployment资源", "number_deleted", deleted)

	return nil
}

// 配置一个Deployment资源
func buildDeployment(myKind mygroupv1beta1.MyKind) *apps.Deployment {
	deployment := apps.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:            myKind.Spec.DeploymentName,
			Namespace:       myKind.Namespace,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(&myKind, mygroupv1beta1.GroupVersion.WithKind("MyKind"))},
		},
		Spec: apps.DeploymentSpec{
			Replicas: myKind.Spec.Replicas,
			// 选择器
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"example-controller.jetstack.io/deployment-name": myKind.Spec.DeploymentName,
				},
			},
			Template: core.PodTemplateSpec{
				// meta
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"example-controller.jetstack.io/deployment-name": myKind.Spec.DeploymentName,
					},
				},
				Spec: core.PodSpec{
					Containers: []core.Container{
						{
							Name:  "nginx",        // 容器名称
							Image: "nginx:latest", // 使用镜像
						},
					},
				},
			},
		},
	}
	return &deployment
}

var (
	deploymentOwnerKey = ".metadata.controller"
)

func (r *MyKindReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(&apps.Deployment{}, deploymentOwnerKey, func(rawObj runtime.Object) []string {
		// grab the Deployment object, extract the owner...
		depl := rawObj.(*apps.Deployment)
		owner := metav1.GetControllerOf(depl)
		if owner == nil {
			return nil
		}
		// ...make sure it's a MyKind...
		if owner.APIVersion != mygroupv1beta1.GroupVersion.String() || owner.Kind != "MyKind" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&mygroupv1beta1.MyKind{}).
		Owns(&apps.Deployment{}).
		Complete(r)
}
