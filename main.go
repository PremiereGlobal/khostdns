package main // import "github.com/PremiereGlobal/khostdns"

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/PremiereGlobal/khostdns/pkg/awsr53"
	"github.com/PremiereGlobal/khostdns/pkg/kubeWatcher"
	"github.com/PremiereGlobal/stim/pkg/stimlog"
	sets "github.com/deckarep/golang-set"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type DNSProvider interface {
	GetAddresses(string) ([]string, error)
	SetAddresses(string, []string) error
	GetCurrentHosts() []string
	GetDNSUpdater() chan string
}

type PromStats struct {
	totalEvents     prometheus.Counter
	dnsUpdateEvents prometheus.Counter
	dnsUpdateErrors prometheus.Counter
}

var log2 stimlog.StimLogger = stimlog.GetLogger() //Fix for deadlock
var log stimlog.StimLogger = stimlog.GetLoggerWithPrefix("Watch")
var version string

func main() {
	lc := stimlog.GetLoggerConfig()
	lc.SetLevel(stimlog.InfoLevel)
	lc.ForceFlush(true)

	config := viper.New()
	config.SetEnvPrefix("khostdns")
	config.AutomaticEnv()
	if version == "" || version == "lastest" {
		version = "unknown"
	}

	var cmd = &cobra.Command{
		Use:   "khostdns",
		Short: "launches the khostdns service",
		Long:  "launches the khostdns service",
		Run: func(cmd *cobra.Command, args []string) {

			if config.GetBool("version") {
				fmt.Printf("%s\n", version)
				os.Exit(0)
			}

			stats := &PromStats{
				totalEvents: promauto.NewCounter(prometheus.CounterOpts{
					Name: "khostdns_events_total",
					Help: "The total number of host/ip notifications",
				}),
				dnsUpdateEvents: promauto.NewCounter(prometheus.CounterOpts{
					Name: "khostdns_dns_changes_total",
					Help: "The total number of DNS modifications made",
				}),
				dnsUpdateErrors: promauto.NewCounter(prometheus.CounterOpts{
					Name: "khostdns_dns_errors_total",
					Help: "The total number of DNS modifications errors",
				}),
			}
			if config.GetString("awszones") == "" {
				log.Warn("No awszones found!")
				cmd.Help()
				os.Exit(1)
			}

			if config.GetString("dnsfilters") == "" {
				log.Warn("No filter found!")
				cmd.Help()
				os.Exit(1)
			}
			switch strings.ToLower(config.GetString("loglevel")) {
			case "info":
				lc.SetLevel(stimlog.InfoLevel)
			case "warn":
				lc.SetLevel(stimlog.WarnLevel)
			case "debug":
				lc.SetLevel(stimlog.DebugLevel)
			case "trace":
				lc.SetLevel(stimlog.TraceLevel)
			}

			czids := strings.Split(config.GetString("awszones"), ",")
			zids := make([]string, 0, len(czids))
			for _, v := range czids {
				if strings.HasPrefix(v, "/hostedzone/") {
					zids = append(zids, v)
				} else if strings.HasPrefix(v, "/") && strings.Count(v, "/") == 1 {
					zids = append(zids, fmt.Sprintf("%s%s", "/hostedzone", v))
				} else if !strings.HasPrefix(v, "/") && strings.Count(v, "/") == 0 {
					zids = append(zids, fmt.Sprintf("%s%s", "/hostedzone/", v))
				} else {
					log.Fatal("Could not parse zone id:{}", v)
				}
			}

			filters := strings.Split(config.GetString("dnsfilters"), ",")

			var dnsP DNSProvider
			dnsP, err := awsr53.NewAWS(zids, filters, time.Minute*5)
			if err != nil {
				log2.Fatal(err)
			}

			kw, err := kubeWatcher.NewKube(filters)
			if err != nil {
				log2.Fatal(err)
			}
			mip := config.GetString("metricsIP")
			if mip == "0.0.0.0" {
				mip = ""
			}
			mipp := fmt.Sprintf("%s:%s", mip, config.GetString("metricsPort"))
			http.Handle("/metrics", promhttp.Handler())
			time.Sleep(1) //Let some stats run before we start
			log2.Info("metrics listening on http://{}", mipp)
			go http.ListenAndServe(mipp, nil)
			startWatching(dnsP, kw, stats)
			for {
				time.Sleep(time.Minute * 5)
			}
		},
	}
	cmd.PersistentFlags().String("awszones", "", "AWS zones to watch (/hostedzone/ZID1,/hostedzone/ZID2)")
	cmd.MarkFlagRequired("awszones")
	config.BindPFlag("awszones", cmd.PersistentFlags().Lookup("awszones"))
	cmd.PersistentFlags().String("dnsfilters", "", "A filter to apply to all RecordSets entries to lookup (us-east-1,clusterb) default is none and will watch all A records in a zone")
	config.BindPFlag("dnsfilters", cmd.PersistentFlags().Lookup("dnsfilters"))
	cmd.PersistentFlags().String("metricsPort", "9347", "prometheus metrics port")
	config.BindPFlag("metricsPort", cmd.PersistentFlags().Lookup("metricsPort"))
	cmd.PersistentFlags().String("metricsIP", "", "IP to listen to metrics on (default is 0.0.0.0 or all IPs)")
	config.BindPFlag("metricsIP", cmd.PersistentFlags().Lookup("metricsIP"))
	cmd.PersistentFlags().String("loglevel", "info", "level to show logs at (warn, info, debug, trace)")
	config.BindPFlag("loglevel", cmd.PersistentFlags().Lookup("loglevel"))
	cmd.PersistentFlags().Bool("version", false, "prints the version the exits")
	config.BindPFlag("version", cmd.PersistentFlags().Lookup("version"))

	err := cmd.Execute()
	if err != nil {
		log2.Fatal(err)
	}
}

func startWatching(dnsP DNSProvider, kw *kubeWatcher.KubeWatcher, stats *PromStats) {

	var currentSleep time.Duration
	defaultSleep := time.Minute
	currentSleep = defaultSleep
	log.Info("----Start Watch Loop----")
	hostSet := sets.NewSet()
	for {
		select {
		case <-time.After(currentSleep):
			if hostSet.Cardinality() == 0 {
				log.Debug("No Changes detected syncing hosts")
				for _, val := range kw.GetCurrentHosts() {
					hostSet.Add(val)
				}
				for _, val := range dnsP.GetCurrentHosts() {
					hostSet.Add(val)
				}
			} else {
				log.Info("Running checks on changed hosts:{}", hostSet.ToSlice())
			}
			hostSet.Each(func(item interface{}) bool {
				checkHost(dnsP, kw, item.(string), stats)
				return false
			})
			hostSet.Clear()
			currentSleep = defaultSleep
		case changedHost := <-kw.GetNotifyChannel():
			stats.totalEvents.Inc()
			log.Debug("Got update from kube for host:{}", changedHost)
			hostSet.Add(changedHost)
			currentSleep = time.Second * 5
		case changedHost := <-dnsP.GetDNSUpdater():
			stats.totalEvents.Inc()
			log.Debug("Got update from DNSProvider for host:{}", changedHost)
			hostSet.Add(changedHost)
			currentSleep = time.Second * 5
		}
	}
}

func checkHost(dnsp DNSProvider, kube *kubeWatcher.KubeWatcher, host string, stats *PromStats) {
	log.Info("Checking Host: --=={}==--", host)
	awsIPs, err := dnsp.GetAddresses(host)
	if err != nil {
		log.Warn("problem getting IPs for dns:{}", err)
		return
	} else {
		log.Debug("dns  Host:{}, IPS: {}", host, awsIPs)
	}
	kubeIPs, err := kube.GetIpsForHost(host)
	if err != nil {
		log.Warn("problem getting IPs for kube:{}", err)
		return
	} else {
		log.Debug("kube Host:{}, IPS: {}", host, kubeIPs)
	}
	if !cmp.Equal(kubeIPs, awsIPs) {
		stats.dnsUpdateEvents.Inc()
		log.Info("Host:{}, Needs updated, kubeIPs:{}, dnsIPs:{}", host, kubeIPs, awsIPs)
		err := dnsp.SetAddresses(host, kubeIPs)
		if err != nil {
			log.Warn("Problems updating DNS:{}", err)
			stats.dnsUpdateErrors.Inc()
		}
	} else {
		log.Info("Host:{}, No changes Needed IPs:{}", host, kubeIPs)
	}
}
