//go:build tools

// Package tools pins dependencies that parallel work streams use, so that no
// one has to run `go get` concurrently. See go.mod.
package tools

import (
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/prometheus/client_golang/prometheus/promhttp"
	_ "golang.org/x/sync/singleflight"
	_ "k8s.io/client-go/informers"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/kubernetes/fake"
	_ "k8s.io/client-go/tools/clientcmd"
	_ "k8s.io/client-go/tools/leaderelection"
	_ "k8s.io/client-go/tools/leaderelection/resourcelock"
)
