package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/ibuildthecloud/wtfk8s/pkg/differ"
	"github.com/ibuildthecloud/wtfk8s/pkg/watcher"
	"github.com/rancher/wrangler/pkg/clients"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

var (
	kubeConfig = flag.String("kubeconfig", "", "Kube config location")
	kcontext   = flag.String("context", "", "Kube config context")
	namespace  = flag.String("namespace", "", "Limit to namespace")
)

func main() {
	klog.InitFlags(nil) // initializing the flags
	defer klog.Flush()  // flushes all pending log I/O

	flag.Parse()
	fmt.Println("ns:", *namespace)

	//klog.SetOutput(io.Discard)
	logrus.SetLevel(logrus.ErrorLevel)

	if err := mainErr(); err != nil {
		log.Fatalln(err)
	}
}

func mainErr() error {

	ctx := signals.SetupSignalContext()
	restConfig := kubeconfig.GetNonInteractiveClientConfigWithContext(*kubeConfig, *kcontext)

	// create the dynamic client from kubeconfig
	//kconfig, _ := restConfig.ClientConfig()
	kubeConfigPath := filepath.Join("/root", ".kube", "config")
	fmt.Printf("Using kubeconfig: %s\n", kubeConfigPath)
	kubeConfig, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return err
	}

	clients, err := clients.New(restConfig, &generic.FactoryOptions{})
	if err != nil {
		return err
	}

	watcher, err := watcher.New(clients, &watcher.Filters{Namespace: *namespace})
	if err != nil {
		return err
	}

	for _, arg := range os.Args[1:] {
		watcher.MatchName(arg)
	}

	differ, err := differ.New(clients)
	if err != nil {
		return err
	}

	objs, err := watcher.Start(ctx)
	if err != nil {
		return err
	}

	go func() {
		for obj := range objs {
			err = differ.Print(obj)
			if err != nil {
				klog.Error(err.Error())
			}
		}
	}()

	if err := clients.Start(ctx); err != nil {
		return err
	}

	//gvk := clients.Apps.DaemonSet().GroupVersionKind()
	//klog.Infof("gvk: %v", gvk)

	time.Sleep(5 * time.Second)
	for _, g := range watcher.GvkList() {
		klog.V(1).Infof("add gvk %v", g)
		err := applyResourceList(watcher, clients, *namespace, "okd-1", g, dynamicClient)
		if err != nil {
			klog.Error(err.Error())
		}
	}

	<-ctx.Done()
	return nil
}

func applyResourceList(w *watcher.Watcher, c *clients.Clients, namespace, newNamespace string, gvk schema.GroupVersionKind, client dynamic.Interface) error {
	mapper, err := c.ToRESTMapper()
	if err != nil {
		return err
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}
	ns := namespace
	switch mapping.Scope.Name() {
	case meta.RESTScopeNameNamespace:
	case meta.RESTScopeNameRoot:
		//ns = ""
		// TODO
		return nil
	default:

	}
	objs, err := c.Dynamic.List(gvk, ns, labels.Everything())
	if err != nil {
		return err
	}

	klog.V(1).Infof("count %d", len(objs))
	for _, obj := range objs {
		if w.MatchFilters(obj) {
			m, err := w.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
			if err != nil {
				return err
			}
			err = apply(client, m, obj, gvk, newNamespace)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func apply(client dynamic.Interface, m *meta.RESTMapping, obj runtime.Object, gvk schema.GroupVersionKind, newNamespace string) error {
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
	default:
	}
	return obj
}
