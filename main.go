package main // import "github.com/PremiereGlobal/khostdns"

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/PremiereGlobal/khostdns/pkg/awsr53"
	"github.com/PremiereGlobal/khostdns/pkg/khostdns"
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

type PromStats struct {
	totalEvents     prometheus.Counter
	dnsUpdateEvents prometheus.Counter
	dnsUpdateErrors prometheus.Counter
}

var log stimlog.StimLogger = stimlog.GetLoggerWithPrefix("Watch")
var version string
var config = viper.New()

func main() {
	lc := stimlog.GetLoggerConfig()
	lc.SetLevel(stimlog.InfoLevel)
	lc.ForceFlush(true)

	config.SetEnvPrefix("khostdns")
	config.AutomaticEnv()
	if version == "" || version == "lastest" {
		version = "unknown"
	}

	var cmd = &cobra.Command{
		Use:   "khostdns",
		Short: "launches the khostdns service",
		Long:  "launches the khostdns service",
		Run:   runCMD,
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
	cmd.PersistentFlags().String("debugaddr", "", "enable debug Address")
	config.BindPFlag("debugaddr", cmd.PersistentFlags().Lookup("debugaddr"))
	cmd.PersistentFlags().Int("dnssync", 10, "time in minutes to do DNS sync")
	config.BindPFlag("dnssync", cmd.PersistentFlags().Lookup("dnssync"))

	err := cmd.Execute()
	if err != nil {
		log.Fatal(err)
	}
}

func runCMD(cmd *cobra.Command, args []string) {
	lc := stimlog.GetLoggerConfig()
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

	if config.GetString("debugaddr") != "" {
		startDebugService(config.GetString("debugaddr"))
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

	dnsDelayTime := time.Duration(config.GetInt("dnssync")) * time.Minute
	log.Info("DNS Sync Delay time: {}", dnsDelayTime)

	filters := strings.Split(config.GetString("dnsfilters"), ",")

	var dnsP khostdns.DNSSetter
	dnsP, err := awsr53.NewAWS(zids, filters, dnsDelayTime)
	if err != nil {
		log.Fatal(err)
	}

	log.Info("Setting up KubeWatcher")
	kw, err := kubeWatcher.NewKube(filters)
	if err != nil {
		log.Fatal(err)
	}
	log.Info("KubeWatcher Setup")
	mip := config.GetString("metricsIP")
	if mip == "0.0.0.0" {
		mip = ""
	}
	mipp := fmt.Sprintf("%s:%s", mip, config.GetString("metricsPort"))
	http.Handle("/metrics", promhttp.Handler())
	time.Sleep(1) //Let some stats run before we start
	log.Info("metrics listening on http://{}", mipp)
	go http.ListenAndServe(mipp, nil)
	startWatching(dnsP, kw, stats)
}

func startWatching(dnsP khostdns.DNSSetter, kw *kubeWatcher.KubeWatcher, stats *PromStats) {
	defaultSleep := time.Minute
	shortSleep := time.Second * 5
	log.Info("----Start Watch Loop----")
	hostSet := sets.NewSet()
	pendingChanges := 0
	delayTimer := time.NewTimer(defaultSleep)
	for {
		select {
		case <-delayTimer.C:
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
			delayTimer.Reset(defaultSleep)
			pendingChanges = 0
		case changedHost := <-kw.GetNotifyChannel():
			stats.totalEvents.Inc()
			log.Debug("Got update from kube for host:{}", changedHost)
			hostSet.Add(changedHost)
			pendingChanges++
			if pendingChanges == 1 {
				resetTimer(delayTimer, shortSleep)
			}
		case changedHost := <-dnsP.GetDNSUpdater():
			stats.totalEvents.Inc()
			log.Debug("Got update from DNSProvider for host:{}", changedHost)
			hostSet.Add(changedHost.GetHostname())
			pendingChanges++
			if pendingChanges == 1 {
				resetTimer(delayTimer, shortSleep)
			}
		}
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	timer.Stop()
	select {
	case <-timer.C:
	default:
	}
	timer.Reset(delay)
}

func checkHost(dnsp khostdns.DNSSetter, kube *kubeWatcher.KubeWatcher, host string, stats *PromStats) {
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
		err := dnsp.SetAddresses(khostdns.CreateArecord(host, kubeIPs))
		if err != nil {
			log.Warn("Problems updating DNS:{}", err)
			stats.dnsUpdateErrors.Inc()
		}
	} else {
		log.Info("Host:{}, No changes Needed IPs:{}", host, kubeIPs)
	}
}

func startDebugService(addr string) {
	dmux := http.NewServeMux()
	dmuxServer := &http.Server{
		Addr:           addr,
		Handler:        dmux,
		MaxHeaderBytes: 256 * 1024,
	}
	dmux.HandleFunc("/debug/pprof/", pprof.Index)
	dmux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	dmux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	dmux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)

	// Manually add support for paths linked to by index page at /debug/pprof/
	dmux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	dmux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	dmux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	dmux.Handle("/debug/pprof/block", pprof.Handler("block"))
	go func() {
		err := dmuxServer.ListenAndServe()
		log.Warn("Got error with debug service:{}", err)
	}()
}
