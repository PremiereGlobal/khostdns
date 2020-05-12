package khostdns // import "github.com/PremiereGlobal/khostdns"
import (
	"fmt"
	"strings"
)

//DNSScraper This is the interface used to query DNS
type DNSScraper interface {
	GetAddresses(string) ([]string, error)
	GetCurrentHosts() []string
	GetDNSUpdater() <-chan Arecord
}

//DNSSetter This is the interface used to Set DNS, we also need the ability to query it also
type DNSSetter interface {
	DNSScraper
	SetAddresses(a Arecord) error
}

//Arecord is an interface describing a Arecord for DNS
type Arecord interface {
	GetHostname() string
	GetIps() []string
}

type arecord struct {
	hostName string
	ips      []string
}

//CreateArecord creates a new Arecord interface to use
func CreateArecord(hostname string, ips []string) Arecord {
	lips := make([]string, len(ips))
	copy(lips, ips)
	return &arecord{hostName: hostname, ips: lips}
}

func (aa *arecord) GetHostname() string {
	return aa.hostName
}

func (aa *arecord) GetIps() []string {
	return aa.ips
}

func (aa *arecord) String() string {
	return fmt.Sprintf("AWSA:[ Host:%s, Ips:%v ]", aa.hostName, aa.ips)
}

type DNSFilter struct {
	dnsfilters []string
}

func CreateDNSFilter(f []string) *DNSFilter {
	nf := make([]string, len(f))
	copy(nf, f)
	return &DNSFilter{dnsfilters: nf}
}

func (df *DNSFilter) CheckDNSFilter(hostName string) error {
	failFilter := len(df.dnsfilters) > 0
	for _, v := range df.dnsfilters {
		if strings.Contains(hostName, v) {
			failFilter = false
		}
	}
	if failFilter {
		return fmt.Errorf("DNS name: %v does match any filters %v", hostName, df.dnsfilters)
	}
	return nil
}
