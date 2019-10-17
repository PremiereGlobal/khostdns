module github.com/PremiereGlobal/khostdns

go 1.12

replace (
	github.com/PremiereGlobal/khostdns => ./
	k8s.io/api => k8s.io/api v0.0.0-20190222213804-5cb15d344471
	k8s.io/apimachinery => k8s.io/apimachinery v0.0.0-20190221213512-86fb29eff628
	k8s.io/client-go => k8s.io/client-go v10.0.0+incompatible
)

require (
	github.com/PremiereGlobal/stim v0.1.2
	github.com/aws/aws-sdk-go v1.20.20
	github.com/deckarep/golang-set v1.7.1
	github.com/google/go-cmp v0.3.1
	github.com/googleapis/gnostic v0.3.0 // indirect
	github.com/gregjones/httpcache v0.0.0-20190611155906-901d90724c79 // indirect
	github.com/imdario/mergo v0.3.8 // indirect
	github.com/json-iterator/go v1.1.7 // indirect
	github.com/peterbourgon/diskv v2.0.1+incompatible // indirect
	github.com/prometheus/client_golang v0.9.3
	github.com/spf13/afero v1.2.2 // indirect
	github.com/spf13/cobra v0.0.5
	github.com/spf13/viper v1.4.0
	golang.org/x/oauth2 v0.0.0-20190604053449-0f29369cfe45 // indirect
	golang.org/x/time v0.0.0-20190921001708-c4c64cad1fd0 // indirect
	k8s.io/api v0.0.0-20190409092523-d687e77c8ae9
	k8s.io/apimachinery v0.0.0-20190409092423-760d1845f48b
	k8s.io/client-go v0.0.0-20190419212732-59781b88d0fa
	k8s.io/klog v1.0.0 // indirect
)
