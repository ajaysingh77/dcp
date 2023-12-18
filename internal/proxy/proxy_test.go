// Copyright (c) Microsoft Corporation. All rights reserved.

package proxy

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/go-logr/logr"
	apiv1 "github.com/microsoft/usvc-apiserver/api/v1"
	"github.com/stretchr/testify/require"
)

func TestInvalidProxyMode(t *testing.T) {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := NewProxy("tcp", "", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)
	err := proxy.Start()
	require.Error(t, err)
}

func TestTCPModeProxy(t *testing.T) {
	// Set up a server
	serverListener, err := net.Listen("tcp", "127.0.0.1:11224")
	require.NoError(t, err)
	require.NotNil(t, serverListener)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := NewProxy(apiv1.TCP, "127.0.0.1", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)

	err = proxy.Start()
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1", proxy.EffectiveAddress)
	require.NotEqual(t, 0, proxy.EffectivePort)

	// Feed the config to the proxy
	config := &ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: 11224},
		},
	}
	proxy.ConfigChanged(config)

	// Set up a client
	clientConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", proxy.EffectiveAddress, proxy.EffectivePort))
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	// Have the client send a message
	_, err = clientConn.Write([]byte("hello"))
	require.NoError(t, err)

	// Have the server receive a message
	proxiedConnection, err := serverListener.Accept()
	require.NoError(t, err)
	require.NotNil(t, proxiedConnection)

	// Verify the message
	buffer := make([]byte, 5)
	_, err = proxiedConnection.Read(buffer)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buffer))

	// Clean up
	proxiedConnection.Close()
	clientConn.Close()
	serverListener.Close()
}

func TestUDPModeProxy(t *testing.T) {
	// Set up a server
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:11225")
	require.NoError(t, err)
	require.NotNil(t, serverConn)

	// Set up a proxy
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	proxy := NewProxy(apiv1.UDP, "127.0.0.1", 0, ctx, logr.Discard())
	require.NotNil(t, proxy)

	err = proxy.Start()
	require.NoError(t, err)

	// Feed the config to the proxy
	config := &ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: 11225},
		},
	}
	proxy.ConfigChanged(config)

	// Set up a client
	clientConn, err := net.Dial("udp", fmt.Sprintf("%s:%d", proxy.EffectiveAddress, proxy.EffectivePort))
	require.NoError(t, err)
	require.NotNil(t, clientConn)

	// Have the client send a message
	_, err = clientConn.Write([]byte("goodbye"))
	require.NoError(t, err)

	// Have the server receive a message
	buffer := make([]byte, 7)
	_, _, err = serverConn.ReadFrom(buffer)
	require.NoError(t, err)

	// Verify the message
	require.Equal(t, "goodbye", string(buffer))

	// Clean up
	clientConn.Close()
	serverConn.Close()
}

func TestSelectRandomEndpoint(t *testing.T) {
	config := &ProxyConfig{
		Endpoints: []Endpoint{
			{Address: "127.0.0.1", Port: 8080},
			{Address: "127.0.0.2", Port: 8081},
		},
	}
	endpoint, err := selectRandomEndpoint(config)
	require.NoError(t, err)
	require.NotNil(t, endpoint)

	config = &ProxyConfig{}
	endpoint, err = selectRandomEndpoint(config)
	require.Error(t, err)
	require.Nil(t, endpoint)
}
