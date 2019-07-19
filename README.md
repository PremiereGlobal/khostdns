# khostdns

khostdns is a tool for dealing with host networked pods and DNS inside of kubernetes.  As this is normally not a good idea some applications do require it.

## Kubernetes Setup

Currently this only works with AWS route53, though other DNS providers could be added.  To use khostdns deploy it into your cluster with the follow Rbac ClusterRole and RoleBinding:

```
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: khostdns
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "watch", "list"]


apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: khostdns
  namespace: khostdns
subjects:
- kind: User
  name: khostdns
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: Role 
  name: khostdns
  apiGroup: rbac.authorization.k8s.io
```

khostdns does not ever write to kubernetes, it only watches pods as they come in and out of the custer.

## Usage

khostdns takes the follow environement variables:

* KHOSTDNS_AWSZONES - A list of the AWS zones to watch (/hostedzone/ZID1,/hostedzone/ZID2).
* KHOSTDNS_DNSFILTERS - a list of filters to apply to the DNS entries to watch, only one has to match for it to watch it.  If none are given it will watch all A records in the given zones.
* KHOSTDNS_METRICSPORT - the port to use for the prometheus metrics, defaults to 9347
* KHOSTDNS_METRICSIP - The IP address to listen on for metrics (default is 0.0.0.0 or all IPs)
* KHOSTDNS_LOGLEVEL - The log level to set khostdns to, default is info (warn, info, debug, trace)

khostdns also currently requires AWS keys are setup.  This can be done with standard aws environment variables or set as default AWS profiles file.


