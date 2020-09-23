package awsr53 // import "github.com/PremiereGlobal/khostdns/pkg/awsr53"

import (
	"sort"
	"time"

	"github.com/PremiereGlobal/khostdns/pkg/khostdns"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/prometheus/client_golang/prometheus"
)

func isAWSThrottleError(err error) bool {
	switch awsErr := err.(type) {
	case awserr.Error:
		switch awsErr.Code() {
		case route53.ErrCodeThrottlingException:
			return true
		}
	default:
	}
	return false
}

func GetAWSZoneInfo(r53 *route53.Route53, dnsf *khostdns.DNSFilter, zid string) (map[string][]string, error) {
	dns := make(map[string][]string)
	timer := prometheus.NewTimer(dnsCheckLatency)
	rrsl := make([]route53.ResourceRecordSet, 0, 10)
	var rso *route53.ListResourceRecordSetsOutput = &route53.ListResourceRecordSetsOutput{}

	for {
		input := &route53.ListResourceRecordSetsInput{
			HostedZoneId:          aws.String(zid),
			StartRecordIdentifier: rso.NextRecordIdentifier,
			StartRecordName:       rso.NextRecordName,
			StartRecordType:       rso.NextRecordType,
		}
		rsoTmp, err := r53.ListResourceRecordSets(input)
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
		for _, rrs := range rso.ResourceRecordSets {
			tmp := *rrs.Name
			name := tmp[:len(tmp)-1]
			if *rrs.Type == "A" && rrs.ResourceRecords != nil && dnsf.CheckDNSFilter(name) == nil {
				rrsl = append(rrsl, *rrs)
				ips := make([]string, 0)
				if cips, ok := dns[name]; ok {
					ips = cips
				}
				for _, rr := range rrs.ResourceRecords {
					ips = append(ips, *rr.Value)
				}
				sort.Strings(ips)
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

func UpdateR53(r53 *route53.Route53, awsa khostdns.Arecord, zid string, retry int, delete bool) (*route53.ChangeResourceRecordSetsOutput, error) {
	timer := prometheus.NewTimer(dnsSetLatency)
	defer timer.ObserveDuration()
	var crrsr *route53.ChangeResourceRecordSetsOutput
	var err error = nil
	for i := 0; i < 10; i++ {
		rr := make([]*route53.ResourceRecord, 0, len(awsa.GetIps()))
		for _, ip := range awsa.GetIps() {
			nrr := &route53.ResourceRecord{Value: aws.String(ip)}
			rr = append(rr, nrr)
		}
		ttl := int64(60)
		//var change *route53.ChangeResourceRecordSetsInput

		change := &route53.ChangeResourceRecordSetsInput{
			HostedZoneId: aws.String(zid),
			ChangeBatch: &route53.ChangeBatch{
				Changes: []*route53.Change{
					{
						Action: aws.String(route53.ChangeActionUpsert),
						ResourceRecordSet: &route53.ResourceRecordSet{
							Name:            aws.String(awsa.GetHostname()),
							TTL:             &ttl,
							Type:            aws.String(route53.RRTypeA),
							ResourceRecords: rr,
						},
					},
				},
			}}
		if delete {
			change.ChangeBatch.Changes[0].Action = aws.String(route53.ChangeActionDelete)
		}
		// fmt.Println(change)
		crrsr, err = r53.ChangeResourceRecordSets(change)
		if err != nil {
			if isAWSThrottleError(err) {
				dnsThrottles.Inc()
				log.Debug("Hit throttle setting dns:{} {}", awsa, err.Error())
				time.Sleep(time.Millisecond * 500)
				continue
			} else {
				dnsErrors.Inc()
				return nil, err
			}
		} else {
			break
		}
	}
	return crrsr, err
}
