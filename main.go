package main

import (
	"context"
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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	clients, err := clients.New(restConfig, &generic.FactoryOptions{
		Namespace: *namespace,
	})
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
		err := getResourceList(watcher, clients, *namespace, g, dynamicClient)
		if err != nil {
			klog.Error(err.Error())
		}
	}

	<-ctx.Done()
	return nil
}

func getResourceList(w *watcher.Watcher, c *clients.Clients, namespace string, gvk schema.GroupVersionKind, client dynamic.Interface) error {
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
		metadata, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		klog.Infof("gvk: %v, name: %v", gvk, metadata.GetName())
		if w.MatchFilters(obj) {
			metadata.SetNamespace("okd-1")
			metadata.SetResourceVersion("")
			metadata.SetManagedFields([]metav1.ManagedFieldsEntry{})

			m, _ := w.Mapper().RESTMapping(gvk.GroupKind(), gvk.Version)
			unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
			if err != nil {
				return err
			}
			klog.Infof("%v", unstructuredObj)

			_, err = client.
				Resource(m.Resource).
				Create(context.TODO(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{})

			if err != nil {
				return err
			}
		}
	}
	return nil
}
