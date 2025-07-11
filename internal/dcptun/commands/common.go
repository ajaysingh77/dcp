package commands

import (
	"time"

	"github.com/microsoft/usvc-apiserver/internal/dcptun"
)

const (
	gracefulShutdownTimeout = 5 * time.Second
)

var (
	tunnelConfig = dcptun.TunnelProxyConfig{}
)
