package main

import (
	"github.com/tilt-dev/tilt-apiserver/pkg/server/builder"
	"k8s.io/klog/v2"
)

func main() {
	builder := builder.NewServerBuilder()
	err := builder.ExecuteCommand()
	if err != nil {
		klog.Fatal(err)
	}
}
