package main

import (
	"flag"
	"fmt"

	"github.com/aojea/networking-controllers/pkg/networkpolicy"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type fakeNetworkPolicer struct{}

func (f fakeNetworkPolicer) Apply(policy networkpolicy.Policy) error {
	fmt.Printf("Apply Network Policy %+v\n", policy)
	return nil
}

func (f fakeNetworkPolicer) Remove(name string) error {
	fmt.Printf("Remove Network Policy %s\n", name)
	return nil
}

func main() {
	var kubeconfig string
	var master string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.Parse()

	// creates the connection
	config, err := clientcmd.BuildConfigFromFlags(master, kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatal(err)
	}

	informersFactory := informers.NewSharedInformerFactory(clientset, 0)

	networkPolicyController := networkpolicy.NewController(
		clientset,
		informersFactory.Networking().V1().NetworkPolicies(),
		informersFactory.Core().V1().Namespaces(),
		informersFactory.Core().V1().Pods(),
		fakeNetworkPolicer{},
	)

	stop := make(chan struct{})
	defer close(stop)

	informersFactory.Start(stop)
	go networkPolicyController.Run(1, stop)

	// Wait forever
	select {}
}
