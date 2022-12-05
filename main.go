package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ibuildthecloud/wtfk8s/pkg/differ"
	"github.com/ibuildthecloud/wtfk8s/pkg/watcher"
	"github.com/ibuildthecloud/wtfk8s/pkg/wclient"
	"github.com/rancher/wrangler/pkg/clients"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/kubeconfig"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"

	"k8s.io/client-go/dynamic"
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
	newNs := "okd-1"
	ctx := signals.SetupSignalContext()
	restConfig := kubeconfig.GetNonInteractiveClientConfigWithContext(*kubeConfig, *kcontext)

	// create the dynamic client from kubeconfig
	kubeConfig, err := restConfig.ClientConfig()
	//kubeConfigPath := filepath.Join("/root", ".kube", "config")
	//fmt.Printf("Using kubeconfig: %s\n", kubeConfigPath)
	//, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath)
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

	wc, err := wclient.New(watcher, clients, dynamicClient)
	if err != nil {
		return err
	}

	go func() {
		for obj := range objs {
			err = differ.Print(obj)
			if err != nil {
				klog.Error(err.Error())
			}
			err = wc.HandleResouceChange(obj, newNs)
		}
	}()

	if err := clients.Start(ctx); err != nil {
		return err
	}

	time.Sleep(5 * time.Second)
	for _, g := range watcher.GvkList() {
		klog.V(1).Infof("add gvk %v", g)
		err := wc.InitResourceList(*namespace, newNs, g)
		if err != nil {
			klog.Error(err.Error())
		}
	}

	<-ctx.Done()
	return nil
}
