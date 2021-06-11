package networkpolicy

import "net"

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
	source          net.IPNet
	sourcePort      int32
	destination     net.IPNet
	destinationPort int32
	protocol        Protocol
	action          Action
}

type Action int

const (
	Drop Action = iota + 1
	Pass
	Filter
)

type Protocol string

const (
	TCPProtocol  Protocol = "tcp"
	UDProtocol   Protocol = "udp"
	SCTPProtocol Protocol = "sctp"
)
