package controllers

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	kube_core "k8s.io/api/core/v1"
	kube_runtime "k8s.io/apimachinery/pkg/runtime"
	kube_types "k8s.io/apimachinery/pkg/types"
	kube_record "k8s.io/client-go/tools/record"
	kube_ctrl "sigs.k8s.io/controller-runtime"
	kube_client "sigs.k8s.io/controller-runtime/pkg/client"
	kube_handler "sigs.k8s.io/controller-runtime/pkg/handler"
	kube_reconile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kube_source "sigs.k8s.io/controller-runtime/pkg/source"

	core_mesh "github.com/kumahq/kuma/pkg/core/resources/apis/mesh"
	"github.com/kumahq/kuma/pkg/core/resources/manager"
	"github.com/kumahq/kuma/pkg/core/resources/store"
	"github.com/kumahq/kuma/pkg/dns"
	"github.com/kumahq/kuma/pkg/dns/vips"
	k8s_common "github.com/kumahq/kuma/pkg/plugins/common/k8s"
	mesh_k8s "github.com/kumahq/kuma/pkg/plugins/resources/k8s/native/api/v1alpha1"
	"github.com/kumahq/kuma/pkg/plugins/runtime/k8s/metadata"
)

// ConfigMapReconciler reconciles a ConfigMap object
type ConfigMapReconciler struct {
	kube_client.Client
	kube_record.EventRecorder
	Scheme            *kube_runtime.Scheme
	Log               logr.Logger
	ResourceManager   manager.ResourceManager
	ResourceConverter k8s_common.Converter
	VIPsAllocator     *dns.VIPsAllocator
	SystemNamespace   string
}

func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req kube_ctrl.Request) (kube_ctrl.Result, error) {
	mesh, ok := vips.MeshFromConfigKey(req.Name)
	if !ok {
		return kube_ctrl.Result{}, nil
	}

	r.Log.V(1).Info("updating VIPs", "mesh", mesh)

	if err := r.VIPsAllocator.CreateOrUpdateVIPConfig(mesh); err != nil {
		if store.IsResourceConflict(err) {
			r.Log.V(1).Info("VIPs were updated in the other place. Retrying")
			return kube_ctrl.Result{Requeue: true}, nil
		}
		return kube_ctrl.Result{}, err
	}

	r.Log.V(1).Info("VIPs updated", "mesh", mesh)

	return kube_ctrl.Result{}, nil
}

func (r *ConfigMapReconciler) SetupWithManager(mgr kube_ctrl.Manager) error {
	return kube_ctrl.NewControllerManagedBy(mgr).
		For(&kube_core.ConfigMap{}).
		Watches(&kube_source.Kind{Type: &kube_core.Service{}}, kube_handler.EnqueueRequestsFromMapFunc(ServiceToConfigMapsMapper(mgr.GetClient(), r.Log, r.SystemNamespace))).
		Watches(&kube_source.Kind{Type: &mesh_k8s.Dataplane{}}, kube_handler.EnqueueRequestsFromMapFunc(DataplaneToMeshMapper(r.Log, r.SystemNamespace, r.ResourceConverter))).
		Watches(&kube_source.Kind{Type: &mesh_k8s.ZoneIngress{}}, kube_handler.EnqueueRequestsFromMapFunc(ZoneIngressToMeshMapper(r.Log, r.SystemNamespace, r.ResourceConverter))).
		Watches(&kube_source.Kind{Type: &mesh_k8s.VirtualOutbound{}}, kube_handler.EnqueueRequestsFromMapFunc(VirtualOutboundToConfigMapsMapper(r.Log, r.SystemNamespace))).
		Watches(&kube_source.Kind{Type: &mesh_k8s.ExternalService{}}, kube_handler.EnqueueRequestsFromMapFunc(ExternalServiceToConfigMapsMapper(r.Log, r.SystemNamespace))).
		Complete(r)
}

func ServiceToConfigMapsMapper(client kube_client.Reader, l logr.Logger, ns string) kube_handler.MapFunc {
	l = l.WithName("service-to-configmap-mapper")
	return func(obj kube_client.Object) []kube_reconile.Request {
		cause, ok := obj.(*kube_core.Service)
		if !ok {
			l.WithValues("dataplane", obj.GetName()).Error(errors.Errorf("wrong argument type: expected %T, got %T", cause, obj), "wrong argument type")
			return nil
		}

		ctx := context.Background()
		svcName := fmt.Sprintf("%s/%s", cause.Namespace, cause.Name)
		// List Pods in the same namespace
		pods := &kube_core.PodList{}
		if err := client.List(ctx, pods, kube_client.InNamespace(obj.GetNamespace())); err != nil {
			l.WithValues("service", svcName).Error(err, "failed to fetch Dataplanes in namespace")
			return nil
		}

		meshSet := map[string]bool{}
		for _, pod := range pods.Items {
			if mesh, exist := metadata.Annotations(pod.Annotations).GetString(metadata.KumaMeshAnnotation); exist {
				meshSet[mesh] = true
			}
		}
		var req []kube_reconile.Request
		for mesh := range meshSet {
			req = append(req, kube_reconile.Request{
				NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(mesh)},
			})
		}

		return req
	}
}

func DataplaneToMeshMapper(l logr.Logger, ns string, resourceConverter k8s_common.Converter) kube_handler.MapFunc {
	l = l.WithName("dataplane-to-mesh-mapper")
	return func(obj kube_client.Object) []kube_reconile.Request {
		cause, ok := obj.(*mesh_k8s.Dataplane)
		if !ok {
			l.WithValues("dataplane", obj.GetName()).Error(errors.Errorf("wrong argument type: expected %T, got %T", cause, obj), "wrong argument type")
			return nil
		}

		dp := core_mesh.NewDataplaneResource()
		if err := resourceConverter.ToCoreResource(cause, dp); err != nil {
			converterLog.Error(err, "failed to parse Dataplane", "dataplane", cause.Spec)
			return nil
		}

		// backwards compatibility
		if dp.Spec.IsIngress() {
			meshSet := map[string]bool{}
			for _, service := range dp.Spec.GetNetworking().GetIngress().GetAvailableServices() {
				meshSet[service.Mesh] = true
			}

			var requests []kube_reconile.Request
			for mesh := range meshSet {
				requests = append(requests, kube_reconile.Request{
					NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(mesh)},
				})
			}
			return requests
		}

		return []kube_reconile.Request{{
			NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(cause.Mesh)},
		}}
	}
}

func ZoneIngressToMeshMapper(l logr.Logger, ns string, resourceConverter k8s_common.Converter) kube_handler.MapFunc {
	l = l.WithName("zone-ingress-to-mesh-mapper")
	return func(obj kube_client.Object) []kube_reconile.Request {
		cause, ok := obj.(*mesh_k8s.ZoneIngress)
		if !ok {
			l.WithValues("zoneIngress", obj.GetName()).Error(errors.Errorf("wrong argument type: expected %T, got %T", cause, obj), "wrong argument type")
			return nil
		}
		zoneIngress := core_mesh.NewZoneIngressResource()
		if err := resourceConverter.ToCoreResource(cause, zoneIngress); err != nil {
			converterLog.Error(err, "failed to parse ZoneIngress", "zoneIngress", cause.Spec)
			return nil
		}

		meshSet := map[string]bool{}
		for _, service := range zoneIngress.Spec.GetAvailableServices() {
			meshSet[service.Mesh] = true
		}

		var requests []kube_reconile.Request
		for mesh := range meshSet {
			requests = append(requests, kube_reconile.Request{
				NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(mesh)},
			})
		}
		return requests
	}
}

func ExternalServiceToConfigMapsMapper(l logr.Logger, ns string) kube_handler.MapFunc {
	l = l.WithName("external-service-to-configmap-mapper")
	return func(obj kube_client.Object) []kube_reconile.Request {
		cause, ok := obj.(*mesh_k8s.ExternalService)
		if !ok {
			l.WithValues("externalService", obj.GetName()).Error(errors.Errorf("wrong argument type: expected %T, got %T", cause, obj), "wrong argument type")
			return nil
		}

		return []kube_reconile.Request{{
			NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(cause.Mesh)},
		}}
	}
}

func VirtualOutboundToConfigMapsMapper(l logr.Logger, ns string) kube_handler.MapFunc {
	l = l.WithName("virtual-outbound-to-configmap-mapper")
	return func(obj kube_client.Object) []kube_reconile.Request {
		cause, ok := obj.(*mesh_k8s.VirtualOutbound)
		if !ok {
			l.WithValues("virtualOutbound", obj.GetName()).Error(errors.Errorf("wrong argument type: expected %T, got %T", cause, obj), "wrong argument type")
			return nil
		}

		return []kube_reconile.Request{{
			NamespacedName: kube_types.NamespacedName{Namespace: ns, Name: vips.ConfigKey(cause.Mesh)},
		}}
	}
}
