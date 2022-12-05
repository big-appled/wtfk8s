package wclient

import (
	"context"
	"encoding/json"

	"github.com/ibuildthecloud/wtfk8s/pkg/rsconst"
	"github.com/ibuildthecloud/wtfk8s/pkg/watcher"
	"github.com/rancher/wrangler/pkg/clients"
	"github.com/rancher/wrangler/pkg/gvk"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type WClient struct {
	w       *watcher.Watcher
	c       *clients.Clients
	dclient dynamic.Interface
}

func New(w *watcher.Watcher,
	c *clients.Clients,
	dclient dynamic.Interface) (*WClient, error) {

	return &WClient{
		w:       w,
		c:       c,
		dclient: dclient,
	}, nil
}

func (wc *WClient) InitResourceList(namespace, newNamespace string, gvk schema.GroupVersionKind) error {
	objs, err := wc.listGvkResources(wc.c, namespace, gvk)
	if err != nil {
		return nil
	}

	klog.V(1).Infof("count %d", len(objs))
	for _, obj := range objs {
		if wc.w.MatchFilters(obj) {
			err = wc.HandleResouceChange(obj, newNamespace)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (wc *WClient) listGvkResources(c *clients.Clients, namespace string, gvk schema.GroupVersionKind) ([]runtime.Object, error) {
	mapper, err := c.ToRESTMapper()
	if err != nil {
		return nil, err
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	ns := namespace
	switch mapping.Scope.Name() {
	case meta.RESTScopeNameNamespace:
	case meta.RESTScopeNameRoot:
		//ns = ""
		// TODO
		return nil, nil
	default:

	}
	objs, err := c.Dynamic.List(gvk, ns, labels.Everything())
	if err != nil {
		return nil, err
	}
	return objs, nil
}

func (wc *WClient) HandleResouceChange(obj runtime.Object, newNamespace string) error {
	if wc.isDeletion(obj) {
		return wc.handleDeletion(obj, newNamespace)
	}

	return wc.applyResource(obj, newNamespace)
}

func (wc *WClient) applyResource(obj runtime.Object, newNamespace string) error {
	gvk, err := gvk.Get(obj)
	if err != nil {
		return err
	}
	m, err := wc.w.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		klog.Error(err.Error())
		return err
	}
	err = apply(wc.dclient, m, obj, newNamespace)
	if err != nil {
		return err
	}

	return nil
}

func (wc *WClient) isDeletion(obj runtime.Object) bool {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return false
	}
	d := metadata.GetDeletionTimestamp()

	return d != nil
}
func (wc *WClient) handleDeletion(obj runtime.Object, newNamespace string) error {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	f := metadata.GetFinalizers()
	d := metadata.GetDeletionTimestamp()
	if d == nil {
		return nil
	}

	// remove old finalizer
	if f != nil {
		n := sets.NewString(f...).Delete(rsconst.FinalizerName).List()
		metadata.SetFinalizers(n)
	}
	// remove from newNamespace
	newObj := obj.DeepCopyObject()
	err = wc.Delete(newObj, newNamespace)
	if err != nil {
		return err
	}

	// update old obj
	klog.Infof("handle deletion of %s", metadata.GetName())
	//_, err = wc.w.Dynamic().Update(obj)
	gvk, err := gvk.Get(obj)
	if err != nil {
		return err
	}
	m, err := wc.w.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		klog.Error(err.Error())
		return err
	}
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	_, err = wc.dclient.Resource(m.Resource).Namespace(metadata.GetNamespace()).Update(context.TODO(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.UpdateOptions{})
	if err != nil {
		klog.Error(err.Error())
	}
	return err
}

func (wc *WClient) Delete(obj runtime.Object, newNamespace string) error {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	gvk, err := gvk.Get(obj)
	if err != nil {
		return err
	}
	m, err := wc.w.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		klog.Error(err.Error())
		return err
	}

	// remove finalizer
	err = apply(wc.dclient, m, obj, newNamespace)
	if err != nil {
		klog.Error(err.Error())
		return err
	}

	err = wc.dclient.Resource(m.Resource).Namespace(newNamespace).Delete(context.TODO(), metadata.GetName(), metav1.DeleteOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	return err
}

func apply(client dynamic.Interface, m *meta.RESTMapping, obj runtime.Object, newNamespace string) error {
	gvk := m.GroupVersionKind
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	klog.Infof("gvk: %v, name: %v", gvk, metadata.GetName())

	existResource, err := client.Resource(m.Resource).Namespace(newNamespace).Get(context.TODO(), metadata.GetName(), metav1.GetOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		return err
	} else if k8serrors.IsNotFound(err) {
		err = create(client, m, obj, metadata, newNamespace)
	} else {
		if existResource == nil || existResource.Object == nil {
			klog.Error("empty object")
			return nil
		}
		existObj := obj.DeepCopyObject()
		err = runtime.DefaultUnstructuredConverter.FromUnstructured(existResource.Object, existObj)
		if err != nil {
			return err
		}
		err = patch(client, m, obj, existObj, newNamespace)
	}

	return err
}

func create(client dynamic.Interface, m *meta.RESTMapping, obj runtime.Object, metadata metav1.Object, newNamespace string) error {
	metadata.SetNamespace(newNamespace)
	metadata.SetResourceVersion("")
	metadata.SetManagedFields([]metav1.ManagedFieldsEntry{})

	newObj := specialHandle(obj, m)
	if newObj == nil {
		return nil
	}
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newObj)
	if err != nil {
		return err
	}
	klog.V(3).Infof("%v", unstructuredObj)
	_, err = client.Resource(m.Resource).Namespace(newNamespace).Create(context.TODO(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{})
	return err
}

func patch(client dynamic.Interface, m *meta.RESTMapping, obj, existObj runtime.Object, newNamespace string) error {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	oldMetadata, err := meta.Accessor(existObj)
	if err != nil {
		return err
	}

	//newObj := existObj.DeepCopyObject()
	newMetadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	// remove deletion, it seems not allowed
	newMetadata.SetDeletionTimestamp(nil)
	newMetadata.SetDeletionGracePeriodSeconds(nil)
	// update label
	newMetadata.SetLabels(metadata.GetLabels())
	// update annotation
	newMetadata.SetAnnotations(metadata.GetAnnotations())
	// update owner
	newMetadata.SetOwnerReferences(metadata.GetOwnerReferences())
	// clean manager field
	newMetadata.SetManagedFields([]metav1.ManagedFieldsEntry{})
	// fetch old uid
	newMetadata.SetUID(oldMetadata.GetUID())
	// fetch old self link
	newMetadata.SetSelfLink(oldMetadata.GetSelfLink())
	// fetch old revision
	newMetadata.SetResourceVersion(oldMetadata.GetResourceVersion())
	// fetch ns
	newMetadata.SetNamespace(oldMetadata.GetNamespace())

	newObj := specialHandle(obj, m)
	klog.V(2).Info("new obj", newObj)

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(newObj)
	if err != nil {
		return err
	}
	klog.V(3).Infof("%v", unstructuredObj)
	_, err = client.Resource(m.Resource).Namespace(newNamespace).Update(context.TODO(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.UpdateOptions{FieldManager: "sync/update"})
	//_, err = client.Resource(m.Resource).Namespace(newNamespace).Apply(context.TODO(), newMetadata.GetName(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.ApplyOptions{Force: true, FieldManager: "sync/apply-patch"})
	return err
}

func specialHandle(obj runtime.Object, m *meta.RESTMapping) runtime.Object {
	switch m.GroupVersionKind.Kind {
	case "Service": // service
		a := obj.(*corev1.Service)
		a.Spec.ClusterIP = ""
		a.Spec.ClusterIPs = []string{}
		a.Kind = "Service"
		a.APIVersion = "v1"
		for i, p := range a.Spec.Ports {
			if p.NodePort != 0 {
				a.Spec.Ports[i].NodePort = 0
			}
		}
		return typeToRuntimeObj(obj, m, a)
	case "Secret":
		a := obj.(*corev1.Secret)
		annotations := a.GetAnnotations()
		if annotations != nil {
			_, ok := annotations["kubernetes.io/service-account.name"]
			if ok {
				return nil
			}
		}
	case "ServiceAccount":
		a := obj.(*corev1.ServiceAccount)
		a.Secrets = nil
		return typeToRuntimeObj(obj, m, a)
	default:
	}

	return obj
}

func typeToRuntimeObj(obj runtime.Object, m *meta.RESTMapping, a client.Object) runtime.Object {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypes(m.Resource.GroupVersion(), obj)
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	b, err := json.Marshal(a)
	if err != nil {
		klog.Error(err.Error())
		return obj
	}
	runtimeObject, _, err := decoder.Decode(b, nil, nil)
	if err != nil {
		klog.Error(err.Error())
		return obj
	}
	return runtimeObject
}
