package kubeWatcher // import "github.com/PremiereGlobal/khostdns/pkg/kubeWatcher"

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PremiereGlobal/stim/pkg/stimlog"
	sets "github.com/deckarep/golang-set"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

type PodInfo struct {
	name     string
	host     string
	lastSeen time.Time
	lastIP   string
}

type KubeWatcher struct {
	kubeHosts     sync.Map
	pods          sync.Map
	lastChecked   time.Time
	dnsfilters    []string
	notifyChannel chan string
	clientset     *kubernetes.Clientset
	dnsChanges    prometheus.Counter
	currentHosts  prometheus.Gauge
}

var log2 stimlog.StimLogger = stimlog.GetLogger() //Fix for deadlock
var log stimlog.StimLogger = stimlog.GetLoggerWithPrefix("KUBE")

func NewKube(dnsfilters []string) (*KubeWatcher, error) {
	var kubeConfig *restclient.Config
	var err error
	home := homeDir()
	kubeConfigPath := filepath.Join(home, ".kube", "config")
	if e, _ := exists(kubeConfigPath); home != "" && e {
		log.Info("Attempting .kube/config")
		// use the current context in kubeconfig
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", kubeConfigPath)
		if err != nil {
			log.Fatal(err.Error())
		}
	} else {
		// Try in-cluster config
		log.Info("Attempting in-cluster config")
		kubeConfig, err = restclient.InClusterConfig()
		if err != nil {
			log.Fatal(err.Error())
		}
	}

	clientset, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		log.Fatal(err.Error())
	}

	kd := &KubeWatcher{
		dnsfilters:    dnsfilters,
		notifyChannel: make(chan string, 20),
		clientset:     clientset,
		dnsChanges: promauto.NewCounter(prometheus.CounterOpts{
			Name: "khostdns_kube_changes_total",
			Help: "The total number of hostdns changes from kubernetes",
		}),
		currentHosts: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "khostdns_kube_hosts_current",
			Help: "The current number of hostdns entries in the cluster",
		}),
	}

	go kd.podsWatcher()
	kd.watchPods()
	return kd, nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func (kd *KubeWatcher) checkDNSFilter(hostName string) error {
	failFilter := len(kd.dnsfilters) > 0
	for _, v := range kd.dnsfilters {
		if strings.Contains(hostName, v) {
			failFilter = false
		}
	}
	if failFilter {
		return errors.New(fmt.Sprintf("DNS name: %v does match any filters %v", hostName, kd.dnsfilters))
	}
	return nil
}

func (kd *KubeWatcher) GetIpsForHost(hostName string) ([]string, error) {
	err := kd.checkDNSFilter(hostName)
	if err != nil {
		return nil, err
	}
	if t, ok := kd.kubeHosts.Load(hostName); ok {
		v := t.(sets.Set)
		rv := make([]string, 0, v.Cardinality())
		v.Each(func(i interface{}) bool {
			rv = append(rv, i.(string))
			return false
		})
		sort.Strings(rv)
		return rv, nil
	} else {
		return nil, errors.New(fmt.Sprintf("Host %s not found in the kubeCluster!", hostName))
	}
}

func (kd *KubeWatcher) GetCurrentHosts() []string {
	keys := make([]string, 0, 10)
	kd.kubeHosts.Range(func(key interface{}, _ interface{}) bool {
		keys = append(keys, key.(string))
		return true
	})
	return keys
}

func (kd *KubeWatcher) GetNotifyChannel() chan string {
	return kd.notifyChannel
}

func (kd *KubeWatcher) podsWatcher() {
	for {
		time.Sleep(time.Minute)
		log.Debug("Running Cleanup")
		cleanup := make([]string, 0, 10)
		kd.pods.Range(func(pn interface{}, podi interface{}) bool {
			name := pn.(string)
			pod := podi.(PodInfo)
			if time.Since(pod.lastSeen) > time.Minute {
				cleanup = append(cleanup, name)
			}
			return true
		})
		if len(cleanup) > 0 {
			for _, v := range cleanup {
				if pi, ok := kd.pods.Load(v); ok {
					pod := pi.(PodInfo)
					log.Info("Cleaning up stuck Pod:{}", pod.name)
					kd.removeIP(pod.name, pod.host, pod.lastIP)
				}
			}
		}
	}
}

func (kd *KubeWatcher) podUpdated(old interface{}, new interface{}) {
	pod := new.(*v1.Pod)
	if hostdns, ok := pod.ObjectMeta.Annotations["hostdns"]; ok {
		externalIP := kd.getPodExternalIP(pod)
		log.Debug("Pod updated: {} - {}:{}", pod.ObjectMeta.Name, hostdns, externalIP)
		if externalIP != "" {
			kd.addnewIP(pod.ObjectMeta.Name, hostdns, externalIP)
		}
	}
}

func (kd *KubeWatcher) podCreated(obj interface{}) {
	pod := obj.(*v1.Pod)
	if hostdns, ok := pod.ObjectMeta.Annotations["hostdns"]; ok {
		externalIP := kd.getPodExternalIP(pod)
		log.Info("Pod created: {} - {}:{}", pod.ObjectMeta.Name, hostdns, externalIP)
		if externalIP != "" {
			kd.addnewIP(pod.ObjectMeta.Name, hostdns, externalIP)
		}
	}
}

func (kd *KubeWatcher) addnewIP(pod_name string, host string, ipa string) {
	if tipl, ok := kd.kubeHosts.LoadOrStore(host, sets.NewSet(ipa)); ok {
		ips := tipl.(sets.Set)
		if ips.Add(ipa) {
			kd.kubeHosts.Store(host, ips)
			kd.dnsChanges.Inc()
			log.Info("host:{} added IP:{}, Current IP list:{}", host, ipa, ips.ToSlice())
			kd.notifyChannel <- host
		}
	} else {
		kd.dnsChanges.Inc()
		log.Info("host:{} added IP:{}, Current IP list:[{}]", host, ipa, ipa)
		kd.notifyChannel <- host
	}
	kd.currentHosts.Set(float64(len(kd.GetCurrentHosts())))
	kd.pods.Store(pod_name, PodInfo{lastIP: ipa, host: host, name: pod_name, lastSeen: time.Now()})
}

func (kd *KubeWatcher) podDeleted(obj interface{}) {
	pod := obj.(*v1.Pod)
	if hostdns, ok := pod.ObjectMeta.Annotations["hostdns"]; ok {
		externalIP := kd.getPodExternalIP(pod)
		log.Info("Pod deleted: {} - {}", pod.ObjectMeta.Name, externalIP)
		if externalIP != "" {
			kd.removeIP(pod.ObjectMeta.Name, hostdns, externalIP)
		}
	}
}

func (kd *KubeWatcher) removeIP(pod_name string, host string, ipa string) {
	if tipl, ok := kd.kubeHosts.Load(host); ok {
		ips := tipl.(sets.Set)
		if ips.Contains(ipa) {
			ips.Remove(ipa)
			kd.kubeHosts.Store(host, ips)
			log.Info("host:{} Deleted IP:{}, Current IP list:{}", host, ipa, ips.ToSlice())
			kd.notifyChannel <- host
		} else {
			log.Info("Asked to delete non-existing IP for host:{} missing IP:{}, Current IP list:{}", host, ipa, ips.ToSlice())
		}
	}
	kd.currentHosts.Set(float64(len(kd.GetCurrentHosts())))
	kd.pods.Delete(pod_name)
}

func (kd *KubeWatcher) getPodExternalIP(pod *v1.Pod) string {
	node, err := kd.clientset.CoreV1().Nodes().Get(pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, na := range node.Status.Addresses {
		if na.Type == v1.NodeExternalIP {
			return na.Address
		}
	}
	return ""
}

func (kd *KubeWatcher) watchPods() {
	resyncPeriod := 30 * time.Second
	si := informers.NewSharedInformerFactory(kd.clientset, resyncPeriod)
	si.Core().V1().Pods().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    kd.podCreated,
			UpdateFunc: kd.podUpdated,
			DeleteFunc: kd.podDeleted,
		},
	)
	si.Start(wait.NeverStop)
}
