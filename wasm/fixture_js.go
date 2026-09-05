//go:build tailserve_test

// Test transport only. This file is excluded from the distributed WASM.
package main

import (
	"context"
	"github.com/coder/websocket"
	"net"
	"syscall/js"
)

func init() {
	testDial = func(ctx context.Context, address string) (net.Conn, error) {
		c, _, err := websocket.Dial(ctx, js.Global().Get("__tailserveTestSocketURL").String(), nil)
		if err != nil {
			return nil, err
		}
		// The SSH connection owns its lifetime, not the dial timeout context.
		return websocket.NetConn(context.Background(), c, websocket.MessageBinary), nil
	}
	testIPNFactory = func(config js.Value) any {
		ipn := &jsIPN{}
		return map[string]any{
			"run": js.FuncOf(func(_ js.Value, args []js.Value) any {
				args[0].Call("notifyState", "Running")
				args[0].Call("notifyNetMap", `{"peers":[{"name":"fixture.internal","online":true,"tailscaleSSHEnabled":false}]}`)
				config.Get("stateStorage").Call("setState", "fixture-node", "00aabb")
				return nil
			}),
			"login": js.FuncOf(func(_ js.Value, _ []js.Value) any { return nil }),
			"ssh":   js.FuncOf(func(_ js.Value, a []js.Value) any { return ipn.ssh(a[0].String(), a[1].String(), a[2]) }),
		}
	}
}
