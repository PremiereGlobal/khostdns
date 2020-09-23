package awsr53 // import "github.com/PremiereGlobal/khostdns/pkg/awsr53"

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/PremiereGlobal/khostdns/pkg/khostdns"
	"github.com/PremiereGlobal/stim/pkg/stimlog"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	sets "github.com/deckarep/golang-set"
	"github.com/google/go-cmp/cmp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var log stimlog.StimLogger = stimlog.GetLoggerWithPrefix("AWS")

var dnsThrottles = promauto.NewCounter(prometheus.CounterOpts{
	Name: "khostdns_aws_throttles_total",
	Help: "The total number of throttles recived from the AWS r53 API",
})
var dnsErrors = promauto.NewCounter(prometheus.CounterOpts{
	Name: "khostdns_aws_errors_total",
	Help: "The total number of errors recived from the AWS r53 API",
})
var currentHosts = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "khostdns_aws_hosts_current",
	Help: "The current number of AWS dns entries controlled by this cluster",
})
var dnsSetLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name: "khostdns_aws_set_dns_seconds",
	Help: "The time it takes to make AWS r53 changes",
})
var dnsCheckLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name: "khostdns_aws_check_dns_seconds",
	Help: "The time it takes to check an AWS r53 DNS zone",
})
var dnsLoopLatency = promauto.NewHistogram(prometheus.HistogramOpts{
	Name: "khostdns_aws_sync_loop_seconds",
	Help: "The time it takes to fully loop over all AWS r53 zones",
})

//AWSData is the main Data Struct for AWSProvider
type AWSData struct {
	awsHosts      sync.Map
	lastChecked   time.Time
	zones         []string
	zoneNames     sync.Map
	dnsfilters    *khostdns.DNSFilter
	delay         time.Duration
	session       *session.Session
	notifyChannel chan khostdns.Arecord
	hostChange    chan khostdns.Arecord
	forceUpdate   chan bool
	r53           *route53.Route53
	running       bool
}

//NewAWS Creates a new AWSProvider for the set zones with the set filters
func NewAWS(zones []string, dnsfilters []string, delay time.Duration) (khostdns.DNSSetter, error) {
	log.Info("Creating new AWS DNSSetter")
	sess, err := session.NewSession(&aws.Config{Region: aws.String("us-east-1")})
	if err != nil {
		return nil, err
	}
	r53 := route53.New(sess)
	ad := &AWSData{
		zones:         zones,
		dnsfilters:    khostdns.CreateDNSFilter(dnsfilters),
		delay:         delay,
		notifyChannel: make(chan khostdns.Arecord),
		hostChange:    make(chan khostdns.Arecord),
		forceUpdate:   make(chan bool),
		session:       sess,
		r53:           r53,
		running:       false,
	}
	m, err := ad.getAWSZoneNames()
	if err != nil {
		return nil, err
	}
	for awsZ, name := range m {
		for _, zid := range ad.zones {
			if zid == awsZ {
				ad.zoneNames.Store(zid, name)
				log.Info("found zone, {}:{}", awsZ, name)
			}
		}
	}
	ad.running = true

	go ad.loop()
	return ad, nil
}

func (ad *AWSData) Shutdown() {
	ad.running = false
	select {
	case ad.forceUpdate <- true:
	case <-time.After(1 * time.Second):
	}
}

func (ad *AWSData) SetAddresses(awsa khostdns.Arecord) error {
	log.Info("Got SetAddress call: HOST:{} IPS:{}", awsa.GetHostname(), awsa.GetIps())
	var err error
	err = ad.dnsfilters.CheckDNSFilter(awsa.GetHostname())
	if err != nil {
		return err
	}
	ad.hostChange <- awsa
	return nil
}

func (ad *AWSData) GetAddresses(hostName string) ([]string, error) {
	err := ad.dnsfilters.CheckDNSFilter(hostName)
	if err != nil {
		return nil, err
	}
	if v, ok := ad.awsHosts.Load(hostName); ok {
		ar := v.(khostdns.Arecord)
		return ar.GetIps(), nil
	}
	return []string{}, nil

}

func (ad *AWSData) GetCurrentHosts() []string {
	keys := make([]string, 0, 10)
	ad.awsHosts.Range(func(key interface{}, _ interface{}) bool {
		keys = append(keys, key.(string))
		return true
	})
	return keys
}

func (ad *AWSData) GetDNSUpdater() <-chan khostdns.Arecord {
	return ad.notifyChannel
}

//Runs once on construction
func (ad *AWSData) getAWSZoneNames() (map[string]string, error) {
	timer := prometheus.NewTimer(dnsCheckLatency)
	defer timer.ObserveDuration()
	var rso *route53.ListHostedZonesOutput = &route53.ListHostedZonesOutput{}
	allZones := make(map[string]string)
	for {
		input := &route53.ListHostedZonesInput{
			Marker: rso.NextMarker,
		}
		rsoTmp, err := ad.r53.ListHostedZones(input)
		if err != nil {
			if isAWSThrottleError(err) {
				dnsThrottles.Inc()
				log.Debug("Hit throttle {}", err.Error())
				continue
			}
			dnsErrors.Inc()
			log.Warn("ERROR: {}", err.Error())
			return nil, err
		}
		rso = rsoTmp
		for _, hz := range rso.HostedZones {
			tmp := *hz.Name
			allZones[*hz.Id] = tmp[:len(tmp)-1]
		}
		if !*rso.IsTruncated {
			break
		}
	}
	return allZones, nil
}

//Main AWS loop on its own goRoutine
func (ad *AWSData) loop() {
	ad.getAWSInfo()

	for ad.running {
		start := time.Now()
		delayTimer := time.NewTimer(ad.delay)
		log.Trace("Loop Start")
		select {
		case ac := <-ad.hostChange:
			log.Trace("Loop:Changing Host")
			err := ad.doSetAddress(ac)
			if err != nil {
				log.Warn("got error setting DNS:{}, {}", ac, err)
			}
		case <-ad.forceUpdate:
			log.Trace("Loop:AWS force Update")
			ad.getAWSInfo()
		case <-delayTimer.C:
			log.Trace("Loop:Hit AWS update timeout")
			err := ad.getAWSInfo()
			if err != nil {
				log.Fatal("Problems talking to AWS:{}", err)
			}
		}
		delayTimer.Stop()
		loopTime := time.Since(start).Seconds()
		dnsLoopLatency.Observe(loopTime)
		log.Trace("Loop End: {}s", fmt.Sprintf("%.4f", loopTime))
	}
	if ad.running {
		log.Warn("Exited main loop w/o shuttdown!")
	}
}

//main setAddress command, runs in main loop
func (ad *AWSData) doSetAddress(awsa khostdns.Arecord) error {
	log.Info("Processing SetAddress call: HOST:{} IPS:{}", awsa.GetHostname(), awsa.GetIps())
	newSet := sets.NewSet()
	for _, v := range awsa.GetIps() {
		newSet.Add(v)
	}

	origSet := sets.NewSet()
	if v, ok := ad.awsHosts.Load(awsa.GetHostname()); ok {
		origList := v.(khostdns.Arecord).GetIps()
		for _, v := range origList {
			origSet.Add(v)
		}
	} else {
		origSet = sets.NewSet()
	}
	log.Info("SetAddress HOST:{} NEW:{} CURRENT:{}", awsa.GetHostname(), newSet, origSet)
	var zid string
	ad.zoneNames.Range(func(key interface{}, value interface{}) bool {
		sv := value.(string)
		if strings.HasSuffix(awsa.GetHostname(), sv) {
			zid = key.(string)
			return false
		}
		return true
	})
	if newSet.Cardinality() == 0 && origSet.Cardinality() > 0 {
		dawsa := khostdns.CreateArecordFromSet(awsa.GetHostname(), origSet)
		log.Info("Deleteing DNS zid:{} host:{} IPS:{}", zid, dawsa.GetHostname(), origSet.ToSlice())

		var crrsr *route53.ChangeResourceRecordSetsOutput
		var err error = nil
		crrsr, err = UpdateR53(ad.r53, dawsa, zid, 10, true)
		if err != nil && crrsr == nil {
			return err
		}
		awsWaitTime := time.Now()
		log.Info("Waiting for Route53 to update host:{}", awsa.GetHostname())
		err = ad.r53.WaitUntilResourceRecordSetsChanged(&route53.GetChangeInput{Id: crrsr.ChangeInfo.Id})
		if err != nil {
			log.Info("Error waiting for AWS:{}", err)
			dnsErrors.Inc()
			return err
		}
		log.Info("Done Setting DNS:{}, took:{}", awsa, time.Since(awsWaitTime))
		ad.awsHosts.Store(awsa.GetHostname(), awsa)
	} else {
		if newSet.SymmetricDifference(origSet).Cardinality() > 0 {

			log.Info("Setting zid:{} host:{} to IPS:{} was:{}", zid, awsa.GetHostname(), newSet.ToSlice(), origSet.ToSlice())

			var crrsr *route53.ChangeResourceRecordSetsOutput
			var err error = nil
			crrsr, err = UpdateR53(ad.r53, awsa, zid, 10, false)
			if err != nil && crrsr == nil {
				return err
			}
			awsWaitTime := time.Now()
			log.Info("Waiting for Route53 to update host:{}", awsa.GetHostname())
			err = ad.r53.WaitUntilResourceRecordSetsChanged(&route53.GetChangeInput{Id: crrsr.ChangeInfo.Id})
			if err != nil {
				log.Info("Error waiting for AWS:{}", err)
				dnsErrors.Inc()
				return err
			}
			log.Info("Done Setting DNS:{}, took:{}", awsa, time.Since(awsWaitTime))
			ad.awsHosts.Store(awsa.GetHostname(), awsa)
		} else {
			log.Info("Setting host:{} already set to IPS:{}", awsa.GetHostname(), origSet.ToSlice())
		}
	}

	return nil
}

//Populates the current needed host info from AWS
func (ad *AWSData) getAWSInfo() error {
	log.Trace("GetInfo Start")
	change := sets.NewSet()
	for _, zid := range ad.zones {
		data, err := GetAWSZoneInfo(ad.r53, ad.dnsfilters, zid)
		if err != nil {
			log.Warn("Problems reading from zone:{}, {}", zid, err)
			return err
		} else {
			for host, s := range data {
				ar := khostdns.CreateArecord(host, s)
				if cips, ok := ad.awsHosts.Load(host); ok {
					cipsl := cips.(khostdns.Arecord).GetIps()
					if !cmp.Equal(cipsl, s) {
						log.Trace("Found host change:{} ips:{}", host, s)
						change.Add(ar)
					}
				} else {
					log.Trace("Found new host:{} ips:{}", host, s)
					change.Add(ar)
				}
				ad.awsHosts.Store(host, ar)
			}
		}
	}
	if change.Cardinality() > 0 {
		change.Each(func(i interface{}) bool {
			ar2 := i.(khostdns.Arecord)
			log.Trace("notify changes:{} ips:{}", ar2.GetHostname(), ar2.GetIps())
			ad.notifyChannel <- ar2
			return false
		})
	}
	log.Trace("GetInfo End")
	return nil
}
