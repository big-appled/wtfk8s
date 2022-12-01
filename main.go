package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ibuildthecloud/wtfk8s/pkg/differ"
	"github.com/ibuildthecloud/wtfk8s/pkg/watcher"
	"github.com/rancher/wrangler/pkg/clients"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		err := getResourceList(watcher, clients, *namespace, g)
		if err != nil {
			klog.Error(err.Error())
		}
	}

	<-ctx.Done()
	return nil
}

func getResourceList(w *watcher.Watcher, c *clients.Clients, namespace string, gvk schema.GroupVersionKind) error {
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

			//_, err = c.Dynamic.Update(obj)
			a, _ := json.Marshal(obj)
			_, err := c.CachedDiscovery.RESTClient().Put().Namespace("okd-1").Body(a).Do(context.TODO()).Get()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
