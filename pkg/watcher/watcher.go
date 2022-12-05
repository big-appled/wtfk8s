package watcher

import (
	"context"
	"strings"
	"sync"

	"github.com/ibuildthecloud/wtfk8s/pkg/rsconst"
	"github.com/rancher/lasso/pkg/dynamic"
	"github.com/rancher/wrangler/pkg/clients"
	"github.com/rancher/wrangler/pkg/kv"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/discovery"
	"k8s.io/klog/v2"
)

type Watcher struct {
	mapper   meta.RESTMapper
	cdi      discovery.CachedDiscoveryInterface
	dynamic  *dynamic.Controller
	matchers []dynamic.GVKMatcher
	filters  *Filters
	gvkList  []schema.GroupVersionKind
}

type Filters struct {
	Namespace string
	Selector  *metav1.LabelSelector
}

func (w Watcher) Dynamic() *dynamic.Controller {
	return w.dynamic
}

func (w Watcher) Mapper() meta.RESTMapper {
	return w.mapper
}

func (w Watcher) GvkList() []schema.GroupVersionKind {
	return w.gvkList
}

func New(clients *clients.Clients, filters *Filters) (*Watcher, error) {
	mapper, err := clients.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	cdi, err := clients.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}

	return &Watcher{
		mapper:  mapper,
		cdi:     cdi,
		dynamic: clients.Dynamic,
		filters: filters,
		gvkList: []schema.GroupVersionKind{},
	}, nil
}

func (w *Watcher) isListWatchable(gvk schema.GroupVersionKind) bool {
	mapping, err := w.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false
	}

	_, resources, err := w.cdi.ServerGroupsAndResources()
	if err != nil {
		return false
	}

	for _, res := range resources {
		if res.GroupVersion == gvk.GroupVersion().String() {
			for _, apiResource := range res.APIResources {
				if apiResource.Name == mapping.Resource.Resource {
					list := false
					watch := false
					for _, verb := range apiResource.Verbs {
						if verb == "list" {
							list = true
						} else if verb == "watch" {
							watch = true
						}
					}
					return list && watch
				}
			}
		}
	}

	return false
}

func (w *Watcher) shouldWatch(gvk schema.GroupVersionKind) bool {
	if !w.isListWatchable(gvk) {
		return false
	}

	if len(w.matchers) == 0 {
		return true
	}

	for _, matcher := range w.matchers {
		if matcher(gvk) {
			return true
		}
	}

	return false
}

func (w *Watcher) Start(ctx context.Context) (chan runtime.Object, error) {
	var chanLock sync.Mutex

	result := make(chan runtime.Object, 100)
	go func() {
		<-ctx.Done()
		chanLock.Lock()
		close(result)
		result = nil
		chanLock.Unlock()
	}()

	w.dynamic.OnChange(ctx, "watcher", w.shouldWatch, func(obj runtime.Object) (runtime.Object, error) {
		chanLock.Lock()
		defer chanLock.Unlock()
		klog.Info("enter watcher")
		if obj != nil && result != nil {
			if w.MatchFilters(obj) {
				err := w.apply(obj)
				if err != nil {
					klog.Error(err.Error())
				}
				result <- obj
			}
		}
		return obj, nil
	})

	return result, nil
}

func (w *Watcher) MatchName(name string) {
	w.matchers = append(w.matchers, func(gvk schema.GroupVersionKind) bool {
		return w.isName(name, gvk)
	})
}

func (w *Watcher) isName(name string, gvk schema.GroupVersionKind) bool {
	w.gvkList = appendGvk(w.gvkList, gvk)
	mapping, err := w.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	klog.V(2).Info(mapping.Resource)
	if err != nil {
		return false
	}

	resource, group := kv.Split(name, ".")
	klog.V(2).Infof("gvk group: %s, group: %s", gvk.Group, group)
	klog.V(2).Infof("should watch: %b", (resource == "*" || mapping.Resource.Resource == resource || strings.ToLower(gvk.Kind) == resource) &&
		gvk.Group == group)
	return (resource == "*" || mapping.Resource.Resource == resource || strings.ToLower(gvk.Kind) == resource) &&
		gvk.Group == group
}

func appendGvk(gvkList []schema.GroupVersionKind, gvk schema.GroupVersionKind) []schema.GroupVersionKind {
	version := map[string]int{"v1beta1": 1, "v1": 2}
	for i, g := range gvkList {
		if g.Group == gvk.Group && g.Kind == gvk.Kind && g.Version == gvk.Version {
			return gvkList
		} else if g.Group == gvk.Group && g.Kind == gvk.Kind && g.Version != gvk.Version {
			if version[g.Version] < version[gvk.Version] {
				gvkList[i].Version = gvk.Version
				return gvkList
			} else {
				return gvkList
			}
		}
	}
	return append(gvkList, gvk)
}

func (w *Watcher) MatchFilters(obj runtime.Object) bool {
	metaAccessor := meta.NewAccessor()
	ns, err := metaAccessor.Namespace(obj)
	if err != nil {
		return false
	}
	klog.V(2).Infof("parsed namespace: %s", ns)
	if ns == w.filters.Namespace {
		labels, err := metaAccessor.Labels(obj)
		if err != nil {
			return false
		}
		if matchLabels(labels, w.filters.Selector) && !isControlled(obj) && !skipName(obj) {
			return true
		}
	}

	return false
}

func matchLabels(labels map[string]string, selector *metav1.LabelSelector) bool {
	return true
}

func isControlled(obj runtime.Object) bool {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return true
	}
	owners := metadata.GetOwnerReferences()
	for _, o := range owners {
		if *o.Controller {
			return true
		}
	}
	return false
}

func skipName(obj runtime.Object) bool {
	knownNames := []string{"kube-root-ca.crt", "default-token-"}
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return true
	}
	for _, n := range knownNames {
		if strings.Contains(n, metadata.GetName()) {
			return true
		}
	}
	return false
}

func (w *Watcher) apply(obj runtime.Object) error {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	f := metadata.GetFinalizers()
	if f == nil || !sets.NewString(f...).Has(rsconst.FinalizerName) {
		if f == nil {
			f = []string{}
		}
		metadata.SetFinalizers(append(f, rsconst.FinalizerName))
		klog.Infof("add finalizer to %s", metadata.GetName())
		_, err = w.dynamic.Update(obj)
		return err
	}

	return nil
}
