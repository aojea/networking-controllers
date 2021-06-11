package networkpolicy

import v1 "k8s.io/api/core/v1"

type NetworkPolicer interface {
	Apply(policy Policy) error
	Remove(name string) error
}

// Policy is defined by a set of ACLs and a default action
// ACLs are not ordered and the result should be the union of them
type Policy struct {
	Name          string
	AccessList    []ACL
	DefaultAction Action
}

// ACL defines an access list action
type ACL struct {
	source          []string
	sourcePort      []int
	destination     []string
	destinationPort []ACLPort
	action          Action
}

type Action string

const (
	DropAction   Action = "drop"
	PassAction   Action = "pass"
	FilterAction Action = "filter"
)

type ACLPort struct {
	port     string
	protocol v1.Protocol
}
