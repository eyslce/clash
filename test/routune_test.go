package main

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eyslce/routune/adapter/outbound"
	C "github.com/eyslce/routune/constant"
	"github.com/eyslce/routune/hub/executor"
	"github.com/eyslce/routune/transport/socks5"
)

const (
	ImageShadowsocks     = "mritd/shadowsocks:latest"
	ImageShadowsocksRust = "ghcr.io/shadowsocks/ssserver-rust:latest"
	ImageVmess           = "v2fly/v2fly-core:latest"
	ImageTrojan          = "trojangfw/trojan:latest"
	ImageTrojanGo        = "p4gefau1t/trojan-go:latest"
	ImageSnell           = "ghcr.io/icpz/snell-server:latest"
	ImageXray            = "teddysun/xray:latest"
)

var (
	waitTime = time.Second
	localIP  = net.ParseIP("127.0.0.1")

	defaultExposedPorts = nat.PortSet{
		"10002/tcp": struct{}{},
		"10002/udp": struct{}{},
	}
	defaultPortBindings = nat.PortMap{
		"10002/tcp": []nat.PortBinding{
			{HostPort: "10002", HostIP: "0.0.0.0"},
		},
		"10002/udp": []nat.PortBinding{
			{HostPort: "10002", HostIP: "0.0.0.0"},
		},
	}
	isDarwin = runtime.GOOS == "darwin"
)

func init() {
	currentDir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	homeDir := filepath.Join(currentDir, "config")
	C.SetHomeDir(homeDir)

	if isDarwin {
		localIP, err = defaultRouteIP()
		if err != nil {
			panic(err)
		}
	}

	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer c.Close()

	list, err := c.ImageList(context.Background(), types.ImageListOptions{All: true})
	if err != nil {
		panic(err)
	}

	imageExist := func(image string) bool {
		for _, item := range list {
			for _, tag := range item.RepoTags {
				if image == tag {
					return true
				}
			}
		}
		return false
	}

	images := []string{
		ImageShadowsocks,
		ImageShadowsocksRust,
		ImageVmess,
		ImageTrojan,
		ImageTrojanGo,
		ImageSnell,
		ImageXray,
	}

	for _, image := range images {
		if imageExist(image) {
			continue
		}

		println("pulling image:", image)
		imageStream, err := c.ImagePull(context.Background(), image, types.ImagePullOptions{})
		if err != nil {
			panic(err)
		}

		io.Copy(io.Discard, imageStream)
	}
}

var clean = `
port: 0
socks-port: 0
mixed-port: 0
redir-port: 0
tproxy-port: 0
dns:
	enable: false
`

func cleanup() {
	parseAndApply(clean)
}

func parseAndApply(cfgStr string) error {
	cfg, err := executor.ParseWithBytes([]byte(cfgStr))
	if err != nil {
		return err
	}

	executor.ApplyConfig(cfg, true)
	return nil
}

func newPingPongPair() (chan []byte, chan []byte, func(t *testing.T) error) {
	pingCh := make(chan []byte)
	pongCh := make(chan []byte)
	test := func(t *testing.T) error {
		defer close(pingCh)
		defer close(pongCh)
		pingOpen := false
		pongOpen := false
		var recv []byte

		for {
			if pingOpen && pongOpen {
				break
			}

			select {
			case recv, pingOpen = <-pingCh:
				assert.True(t, pingOpen)
				assert.Equal(t, []byte("ping"), recv)
			case recv, pongOpen = <-pongCh:
				assert.True(t, pongOpen)
				assert.Equal(t, []byte("pong"), recv)
			case <-time.After(10 * time.Second):
				return errors.New("timeout")
			}
		}
		return nil
	}

	return pingCh, pongCh, test
}

func newLargeDataPair() (chan hashPair, chan hashPair, func(t *testing.T) error) {
	pingCh := make(chan hashPair)
	pongCh := make(chan hashPair)
	test := func(t *testing.T) error {
		defer close(pingCh)
		defer close(pongCh)
		pingOpen := false
		pongOpen := false
		var serverPair hashPair
		var clientPair hashPair

		for {
			if pingOpen && pongOpen {
				break
			}

			select {
			case serverPair, pingOpen = <-pingCh:
				assert.True(t, pingOpen)
			case clientPair, pongOpen = <-pongCh:
				assert.True(t, pongOpen)
			case <-time.After(10 * time.Second):
				return errors.New("timeout")
			}
		}

		assert.Equal(t, serverPair.recvHash, clientPair.sendHash)
		assert.Equal(t, serverPair.sendHash, clientPair.recvHash)

		return nil
	}

	return pingCh, pongCh, test
}

func testPingPongWithSocksPort(t *testing.T, port int) error {
	l, err := Listen("tcp", ":10001")
	require.NoError(t, err)
	defer l.Close()

	pingCh, pongCh, test := newPingPongPair()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}

		buf := make([]byte, 4)
		if _, err = io.ReadFull(c, buf); err != nil {
			return
		}

		pingCh <- buf
		if _, err = c.Write([]byte("pong")); err != nil {
			return
		}
	}()

	go func() {
		c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		require.NoError(t, err)
		defer c.Close()

		if _, err = socks5.ClientHandshake(c, socks5.ParseAddr("127.0.0.1:10001"), socks5.CmdConnect, nil); err != nil {
			return
		}

		if _, err = c.Write([]byte("ping")); err != nil {
			return
		}

		buf := make([]byte, 4)
		if _, err = io.ReadFull(c, buf); err != nil {
			return
		}

		pongCh <- buf
	}()

	return test(t)
}

func testPingPongWithConn(t *testing.T, c net.Conn) error {
	l, err := Listen("tcp", ":10001")
	if err != nil {
		return err
	}
	defer l.Close()

	pingCh, pongCh, test := newPingPongPair()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}

		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}

		pingCh <- buf
		if _, err := c.Write([]byte("pong")); err != nil {
			return
		}
	}()

	go func() {
		if _, err := c.Write([]byte("ping")); err != nil {
			return
		}

		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}

		pongCh <- buf
	}()

	return test(t)
}

func testPingPongWithPacketConn(t *testing.T, pc net.PacketConn) error {
	l, err := ListenPacket("udp", ":10001")
	require.NoError(t, err)
	defer l.Close()

	rAddr := &net.UDPAddr{IP: localIP, Port: 10001}

	pingCh, pongCh, test := newPingPongPair()
	go func() {
		buf := make([]byte, 1024)
		n, rAddr, err := l.ReadFrom(buf)
		if err != nil {
			return
		}

		pingCh <- buf[:n]
		if _, err := l.WriteTo([]byte("pong"), rAddr); err != nil {
			return
		}
	}()

	go func() {
		if _, err := pc.WriteTo([]byte("ping"), rAddr); err != nil {
			return
		}

		buf := make([]byte, 1024)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}

		pongCh <- buf[:n]
	}()

	return test(t)
}

type hashPair struct {
	sendHash map[int][]byte
	recvHash map[int][]byte
}

func testLargeDataWithConn(t *testing.T, c net.Conn) error {
	l, err := Listen("tcp", ":10001")
	require.NoError(t, err)
	defer l.Close()

	times := 100
	chunkSize := int64(64 * 1024)

	pingCh, pongCh, test := newLargeDataPair()
	writeRandData := func(conn net.Conn) (map[int][]byte, error) {
		buf := make([]byte, chunkSize)
		hashMap := map[int][]byte{}
		for i := 0; i < times; i++ {
			if _, err := rand.Read(buf[1:]); err != nil {
				return nil, err
			}
			buf[0] = byte(i)

			hash := md5.Sum(buf)
			hashMap[i] = hash[:]

			if _, err := conn.Write(buf); err != nil {
				return nil, err
			}
		}

		return hashMap, nil
	}

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		hashMap := map[int][]byte{}
		buf := make([]byte, chunkSize)

		for i := 0; i < times; i++ {
			_, err := io.ReadFull(c, buf)
			if err != nil {
				t.Log(err.Error())
				return
			}

			hash := md5.Sum(buf)
			hashMap[int(buf[0])] = hash[:]
		}

		sendHash, err := writeRandData(c)
		if err != nil {
			t.Log(err.Error())
			return
		}

		pingCh <- hashPair{
			sendHash: sendHash,
			recvHash: hashMap,
		}
	}()

	go func() {
		sendHash, err := writeRandData(c)
		if err != nil {
			t.Log(err.Error())
			return
		}

		hashMap := map[int][]byte{}
		buf := make([]byte, chunkSize)

		for i := 0; i < times; i++ {
			_, err := io.ReadFull(c, buf)
			if err != nil {
				t.Log(err.Error())
				return
			}

			hash := md5.Sum(buf)
			hashMap[int(buf[0])] = hash[:]
		}

		pongCh <- hashPair{
			sendHash: sendHash,
			recvHash: hashMap,
		}
	}()

	return test(t)
}

func testLargeDataWithPacketConn(t *testing.T, pc net.PacketConn) error {
	l, err := ListenPacket("udp", ":10001")
	require.NoError(t, err)
	defer l.Close()

	rAddr := &net.UDPAddr{IP: localIP, Port: 10001}

	times := 50
	chunkSize := int64(1024)

	pingCh, pongCh, test := newLargeDataPair()
	writeRandData := func(pc net.PacketConn, addr net.Addr) (map[int][]byte, error) {
		hashMap := map[int][]byte{}
		mux := sync.Mutex{}
		for i := 0; i < times; i++ {
			go func(idx int) {
				buf := make([]byte, chunkSize)
				if _, err := rand.Read(buf[1:]); err != nil {
					t.Log(err.Error())
					return
				}
				buf[0] = byte(idx)

				hash := md5.Sum(buf)
				mux.Lock()
				hashMap[idx] = hash[:]
				mux.Unlock()

				if _, err := pc.WriteTo(buf, addr); err != nil {
					t.Log(err.Error())
					return
				}
			}(i)
		}

		return hashMap, nil
	}

	go func() {
		var rAddr net.Addr
		hashMap := map[int][]byte{}
		buf := make([]byte, 64*1024)

		for i := 0; i < times; i++ {
			_, rAddr, err = l.ReadFrom(buf)
			if err != nil {
				t.Log(err.Error())
				return
			}

			hash := md5.Sum(buf[:chunkSize])
			hashMap[int(buf[0])] = hash[:]
		}

		sendHash, err := writeRandData(l, rAddr)
		if err != nil {
			t.Log(err.Error())
			return
		}

		pingCh <- hashPair{
			sendHash: sendHash,
			recvHash: hashMap,
		}
	}()

	go func() {
		sendHash, err := writeRandData(pc, rAddr)
		if err != nil {
			t.Log(err.Error())
			return
		}

		hashMap := map[int][]byte{}
		buf := make([]byte, 64*1024)

		for i := 0; i < times; i++ {
			_, _, err := pc.ReadFrom(buf)
			if err != nil {
				t.Log(err.Error())
				return
			}

			hash := md5.Sum(buf[:chunkSize])
			hashMap[int(buf[0])] = hash[:]
		}

		pongCh <- hashPair{
			sendHash: sendHash,
			recvHash: hashMap,
		}
	}()

	return test(t)
}

func testPacketConnTimeout(t *testing.T, pc net.PacketConn) error {
	err := pc.SetReadDeadline(time.Now().Add(time.Millisecond * 300))
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, _, err := pc.ReadFrom(buf)
		errCh <- err
	}()

	select {
	case <-errCh:
		return nil
	case <-time.After(time.Second * 10):
		return errors.New("timeout")
	}
}

func testSuit(t *testing.T, proxy C.ProxyAdapter) {
	conn, err := proxy.DialContext(context.Background(), &C.Metadata{
		Host:    localIP.String(),
		DstPort: 10001,
	})
	require.NoError(t, err)
	defer conn.Close()
	assert.NoError(t, testPingPongWithConn(t, conn))

	conn, err = proxy.DialContext(context.Background(), &C.Metadata{
		Host:    localIP.String(),
		DstPort: 10001,
	})
	require.NoError(t, err)
	defer conn.Close()
	assert.NoError(t, testLargeDataWithConn(t, conn))

	if !proxy.SupportUDP() {
		return
	}

	pc, err := proxy.ListenPacketContext(context.Background(), &C.Metadata{
		NetWork: C.UDP,
		DstIP:   localIP,
		DstPort: 10001,
	})
	require.NoError(t, err)
	defer pc.Close()

	assert.NoError(t, testPingPongWithPacketConn(t, pc))

	pc, err = proxy.ListenPacketContext(context.Background(), &C.Metadata{
		NetWork: C.UDP,
		DstIP:   localIP,
		DstPort: 10001,
	})
	require.NoError(t, err)
	defer pc.Close()

	assert.NoError(t, testLargeDataWithPacketConn(t, pc))

	pc, err = proxy.ListenPacketContext(context.Background(), &C.Metadata{
		NetWork: C.UDP,
		DstIP:   localIP,
		DstPort: 10001,
	})
	require.NoError(t, err)
	defer pc.Close()

	assert.NoError(t, testPacketConnTimeout(t, pc))
}

func benchmarkProxy(b *testing.B, proxy C.ProxyAdapter) {
	l, err := Listen("tcp", ":10001")
	require.NoError(b, err)
	defer l.Close()

	chunkSize := int64(16 * 1024)
	chunk := make([]byte, chunkSize)
	rand.Read(chunk)

	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()

		go func() {
			for {
				_, err := c.Write(chunk)
				if err != nil {
					return
				}
			}
		}()
		io.Copy(io.Discard, c)
	}()

	conn, err := proxy.DialContext(context.Background(), &C.Metadata{
		Host:    localIP.String(),
		DstPort: 10001,
	})
	require.NoError(b, err)

	_, err = conn.Write([]byte("skip protocol handshake"))
	require.NoError(b, err)

	b.Run("Write", func(b *testing.B) {
		b.SetBytes(chunkSize)
		for i := 0; i < b.N; i++ {
			conn.Write(chunk)
		}
	})

	b.Run("Read", func(b *testing.B) {
		b.SetBytes(chunkSize)
		buf := make([]byte, chunkSize)
		for i := 0; i < b.N; i++ {
			io.ReadFull(conn, buf)
		}
	})
}

func TestRoutune_Basic(t *testing.T) {
	basic := `
mixed-port: 10000
log-level: silent
`

	err := parseAndApply(basic)
	require.NoError(t, err)
	defer cleanup()

	require.True(t, TCPing(net.JoinHostPort("127.0.0.1", "10000")))
	require.NoError(t, testPingPongWithSocksPort(t, 10000))
}

func Benchmark_Direct(b *testing.B) {
	proxy := outbound.NewDirect()
	benchmarkProxy(b, proxy)
}
