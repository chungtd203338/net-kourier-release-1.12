package region

import (
	"context"
	"fmt"
	"strings"

	_ "net/http/pprof"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func InitRegions() []map[string]string {
	return []map[string]string{
		{
			"id":    "0",
			"name":  "region1",
			"label": "node1",
		},
		{
			"id":    "1",
			"name":  "region2",
			"label": "node2",
		},
		{
			"id":    "2",
			"name":  "region3",
			"label": "node3",
		},
	}
}

func Mode() int {
	return 2
}

func Check() int {
	x := 0
	config, err := rest.InClusterConfig()
	// home := homedir.HomeDir()
	// config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	pods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Failed to list pods: %v\n", err)
	}

	for _, pod := range pods.Items {
		if strings.Contains(pod.Name, "hello") {
			x = x + 1
		}
	}
	return x
}
