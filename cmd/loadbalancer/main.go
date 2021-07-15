package main

import (
	"flag"
	"net"

	"github.com/aojea/networking-controllers/pkg/loadbalancer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

func main() {
	var kubeconfig string
	var master string
	var iprange string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	flag.StringVar(&master, "master", "", "master url")
	flag.StringVar(&iprange, "iprange", "", "ip range to allocate loadbalancer ips")

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
	_, cidr, err := net.ParseCIDR(iprange)
	if err != nil {
		klog.Fatal(err)
	}
	informer := informers.NewSharedInformerFactory(clientset, 0)
	lbController := loadbalancer.NewController(
		cidr,
		clientset,
		informer.Core().V1().Services(),
	)
	stopCh := make(chan struct{})
	defer close(stopCh)

	informer.Start(stopCh)
	go lbController.Run(1, stopCh)

	// Wait forever
	select {}
}
