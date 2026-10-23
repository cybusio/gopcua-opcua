package server

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Every reset before Hello is an error of that one connection: after several
// of them the server still accepts each new client.
func TestServer_KeepsAcceptingAfterRepeatedResetsBeforeHello(t *testing.T) {
	endpoint := startAcceptTestServer(t)

	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", hostPort(endpoint))
		require.NoError(t, err)
		require.NoError(t, c.(*net.TCPConn).SetLinger(0)) // close with RST
		require.NoError(t, c.Close())
	}
	time.Sleep(100 * time.Millisecond) // let the server read the resets

	for i := 0; i < 3; i++ {
		requireAnswers(t, endpoint, 3*time.Second)
	}
}

// Silent clients hold no slot in the accept loop: with many of them connected
// at once, another client still completes its handshake.
func TestServer_AnswersOthersWhileManyClientsStaySilent(t *testing.T) {
	endpoint := startAcceptTestServer(t)

	const silent = 20
	var wg sync.WaitGroup
	conns := make(chan net.Conn, silent)
	for i := 0; i < silent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.Dial("tcp", hostPort(endpoint))
			if err != nil {
				t.Errorf("dial: %v", err)
				return
			}
			conns <- c
		}()
	}
	wg.Wait()
	close(conns)
	t.Cleanup(func() {
		for c := range conns {
			_ = c.Close()
		}
	})
	time.Sleep(100 * time.Millisecond) // let the server accept them first

	requireAnswers(t, endpoint, 3*time.Second)
}
