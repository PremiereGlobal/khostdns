package awsr53 // import "github.com/PremiereGlobal/khostdns/pkg/awsr53"

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/PremiereGlobal/stim/pkg/stimlog"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
	sets "github.com/deckarep/golang-set"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type AWSData struct {
	awsHosts        sync.Map
	lastChecked     time.Time
	zones           []string
	zoneNames       sync.Map
	dnsfilters      []string
	delay           time.Duration
	session         *session.Session
	notifyChannel   chan string
	lockUpdate      *sync.Mutex
	forceUpdate     chan bool
	r53             *route53.Route53
	dnsErrors       prometheus.Counter
	dnsThrottles    prometheus.Counter
	dnsSetLatency   prometheus.Histogram
	dnsCheckLatency prometheus.Histogram
	dnsLoopLatency  prometheus.Histogram
	currentHosts    prometheus.Gauge
}

var log stimlog.StimLogger = stimlog.GetLogger()

func NewAWS(zones []string, dnsfilters []string, delay time.Duration) (*AWSData, error) {
	sess, err := session.NewSession(&aws.Config{Region: aws.String("us-east-1")})
	if err != nil {
		return nil, err
	}
	r53 := route53.New(sess)
	ad := &AWSData{
		zones:         zones,
		dnsfilters:    dnsfilters,
		delay:         delay,
		lockUpdate:    &sync.Mutex{},
		notifyChannel: make(chan string, 20),
		forceUpdate:   make(chan bool),
		session:       sess,
		r53:           r53,
		dnsThrottles: promauto.NewCounter(prometheus.CounterOpts{
			Name: "khostdns_aws_throttles_total",
			Help: "The total number of throttles recived from the AWS r53 API",
		}),
		dnsErrors: promauto.NewCounter(prometheus.CounterOpts{
			Name: "khostdns_aws_errors_total",
			Help: "The total number of errors recived from the AWS r53 API",
		}),
		currentHosts: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "khostdns_aws_hosts_current",
			Help: "The current number of AWS dns entries controlled by this cluster",
		}),
		dnsSetLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "khostdns_aws_set_dns_seconds",
			Help: "The time it takes to make AWS r53 changes",
		}),
		dnsCheckLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "khostdns_aws_check_dns_seconds",
			Help: "The time it takes to check an AWS r53 DNS zone",
		}),
		dnsLoopLatency: promauto.NewHistogram(prometheus.HistogramOpts{
			Name: "khostdns_aws_sync_loop_seconds",
			Help: "The time it takes to fully loop over all AWS r53 zones",
		}),
	}
	m, err := ad.getAWSZoneNames()
	for aws_z, name := range m {
		for _, zid := range ad.zones {
			if zid == aws_z {
				ad.zoneNames.Store(zid, name)
				log.Info("AWS: found zone, {}:{}", aws_z, name)
			}
		}
	}
	go ad.getAWSInfo()
	return ad, nil
}

func (ad *AWSData) checkDNSFilter(hostName string) error {
	failFilter := len(ad.dnsfilters) > 0
	for _, v := range ad.dnsfilters {
		if strings.Contains(hostName, v) {
			failFilter = false
		}
	}
	if failFilter {
		return errors.New(fmt.Sprintf("DNS name: %v does match any filters %v", hostName, ad.dnsfilters))
	}
	return nil
}

func (ad *AWSData) SetAddresses(hostName string, ips []string) error {
	var err error
	err = ad.checkDNSFilter(hostName)
	if err != nil {
		return err
	}
	ad.lockUpdate.Lock()
	defer ad.lockUpdate.Unlock()

	newSet := sets.NewSet()
	for _, v := range ips {
		newSet.Add(v)
	}
	var origSet sets.Set
	if v, ok := ad.awsHosts.Load(hostName); ok {
		origSet = v.(sets.Set)
	} else {
		origSet = sets.NewSet()
	}
	if origSet.Difference(newSet).Cardinality() > 0 {
		var zid string
		ad.zoneNames.Range(func(key interface{}, value interface{}) bool {
			sv := value.(string)
			if strings.HasSuffix(hostName, sv) {
				zid = key.(string)
				return false
			}
			return true
		})
		log.Info("AWS: Setting zid:{} host:{} to IPS:{} was:{}", zid, hostName, newSet.ToSlice(), origSet.ToSlice())
		start := time.Now()
		rr := make([]*route53.ResourceRecord, 0, len(ips))
		for _, ip := range ips {
			nrr := &route53.ResourceRecord{Value: aws.String(ip)}
			rr = append(rr, nrr)
		}
		ttl := int64(60)
		change := &route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(zid),
			ChangeBatch: &route53.ChangeBatch{
				Changes: []*route53.Change{
					{
						Action: aws.String(route53.ChangeActionUpsert),
						ResourceRecordSet: &route53.ResourceRecordSet{
							Name:            aws.String(hostName),
							TTL:             &ttl,
							Type:            aws.String(route53.RRTypeA),
							ResourceRecords: rr,
						},
					},
				},
			}}
		var crrsr *route53.ChangeResourceRecordSetsOutput
		for i := 0; i < 10; i++ {
			crrsr, err = ad.r53.ChangeResourceRecordSets(change)
			if err != nil {
				if strings.Contains(err.Error(), "Throttling:") {
					ad.dnsThrottles.Inc()
					log.Warn("AWS: Hit throttle setting dns:{} {}", hostName, err.Error())
					time.Sleep(time.Millisecond * 500)
					continue
				}
				ad.dnsErrors.Inc()
				log.Warn("AWS: error updating DNS for:{}, error:{}", hostName, err)
				return err
			} else {
				break
			}
		}
		if err != nil && crrsr == nil {
			log.Warn("AWS: Hit Final throttle setting dns, aborting:{} {}", hostName, err.Error())
			return err
		}
		log.Info("AWS: Waiting for Route53 to update host:{}", hostName)
		err := ad.r53.WaitUntilResourceRecordSetsChanged(&route53.GetChangeInput{Id: crrsr.ChangeInfo.Id})
		if err != nil {
			ad.dnsErrors.Inc()
			log.Warn("AWS: got error waiting for DNS update:{}, error:{}", hostName, err)
			return err
		}
		log.Info("AWS: Done Waiting for Route53 to update host:{}", hostName)

		sec_l := math.Round(float64(time.Since(start).Nanoseconds())/1000000.0) / 1000.0
		ad.dnsSetLatency.Observe(sec_l)

		ad.awsHosts.Store(hostName, newSet)
		ad.forceUpdate <- true
	} else {
		log.Info("AWS: Setting host:{} already set to IPS:{}", hostName, origSet.ToSlice())
	}

	return nil
}

func (ad *AWSData) GetAddresses(hostName string) ([]string, error) {
	err := ad.checkDNSFilter(hostName)
	if err != nil {
		return nil, err
	}
	if v, ok := ad.awsHosts.Load(hostName); ok {
		ips := v.(sets.Set)
		ip_a := make([]string, 0, ips.Cardinality())
		ips.Each(func(i interface{}) bool {
			ip_a = append(ip_a, i.(string))
			return false
		})
		return ip_a, nil
	} else {
		return []string{}, nil
	}
}

func (ad *AWSData) GetCurrentHosts() []string {
	keys := make([]string, 0, 10)
	ad.awsHosts.Range(func(key interface{}, _ interface{}) bool {
		keys = append(keys, key.(string))
		return true
	})
	return keys
}

func (ad *AWSData) GetDNSUpdater() chan string {
	return ad.notifyChannel
}

func (ad *AWSData) getAWSZoneNames() (map[string]string, error) {
	ad.lockUpdate.Lock()
	defer ad.lockUpdate.Unlock()
	timer := prometheus.NewTimer(ad.dnsCheckLatency)
	defer timer.ObserveDuration()
	var rso *route53.ListHostedZonesOutput = &route53.ListHostedZonesOutput{}
	allZones := make(map[string]string)
	for {
		input := &route53.ListHostedZonesInput{
			Marker: rso.NextMarker,
		}
		rsoTmp, err := ad.r53.ListHostedZones(input)
		if err != nil {
			ad.dnsErrors.Inc()
			if awsErr, ok := err.(awserr.Error); ok {
				switch awsErr.Code() {
				case route53.ErrCodeThrottlingException:
					log.Debug("Hit throttle {}", err.Error())
					continue
				default:
				}
			}
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

func (ad *AWSData) getAWSInfo() {
	for {
		log.Trace("AWS: Loop Start")
		start := time.Now()
		change := sets.NewSet()
		for _, zid := range ad.zones {
			data, err := ad.getAWSZoneInfo(zid)
			if err != nil {
				log.Warn("Problems reading from zone:{}, {}", zid, err)
			} else {
				for host, s := range data {
					if cips, ok := ad.awsHosts.Load(host); ok {
						cips_set := cips.(sets.Set)
						dips := cips_set.Difference(s)
						if dips.Cardinality() > 0 {
							change.Add(host)
						}
					} else {
						change.Add(host)
					}
					ad.awsHosts.Store(host, s)
				}
			}
		}
		loopTime := time.Since(start).Seconds()
		ad.dnsLoopLatency.Observe(loopTime)
		log.Trace("AWS: Loop End: {}s", fmt.Sprintf("%.4f", loopTime))
		if change.Cardinality() > 0 {
			change.Each(func(i interface{}) bool {
				ad.notifyChannel <- i.(string)
				return false
			})
		}
		select {
		case <-ad.forceUpdate:
		case <-time.After(ad.delay - time.Since(start)):
		}
	}
}

func (ad *AWSData) getAWSZoneInfo(zid string) (map[string]sets.Set, error) {
	ad.lockUpdate.Lock()
	defer ad.lockUpdate.Unlock()
	dns := make(map[string]sets.Set)
	timer := prometheus.NewTimer(ad.dnsCheckLatency)
	rrsl := make([]route53.ResourceRecordSet, 0, 10)
	var rso *route53.ListResourceRecordSetsOutput = &route53.ListResourceRecordSetsOutput{}

	for {
		input := &route53.ListResourceRecordSetsInput{
			HostedZoneId:          aws.String(zid),
			StartRecordIdentifier: rso.NextRecordIdentifier,
			StartRecordName:       rso.NextRecordName,
			StartRecordType:       rso.NextRecordType,
		}
		rsoTmp, err := ad.r53.ListResourceRecordSets(input)
		if err != nil {
			if strings.Contains(err.Error(), "Throttling:") {
				ad.dnsThrottles.Inc()
				log.Debug("Hit throttle {}", err.Error())
				continue
			}
			ad.dnsErrors.Inc()
			log.Warn("ERROR: {}", err.Error())
			return nil, err
		}
		rso = rsoTmp
		for _, rrs := range rso.ResourceRecordSets {
			tmp := *rrs.Name
			name := tmp[:len(tmp)-1]
			if *rrs.Type == "A" && rrs.ResourceRecords != nil && ad.checkDNSFilter(name) == nil {
				rrsl = append(rrsl, *rrs)
				var ips sets.Set
				if cips, ok := dns[name]; ok {
					ips = cips
				} else {
					ips = sets.NewSet()
					dns[name] = ips
				}
				for _, rr := range rrs.ResourceRecords {
					ips.Add(*rr.Value)
				}
				dns[name] = ips
			}
		}
		if !*rso.IsTruncated {
			break
		}
	}
	timer.ObserveDuration()
	return dns, nil
}
